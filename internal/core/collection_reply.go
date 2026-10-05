package core

import "github.com/brandopakel/keel/internal/data_structure"

// A replyWalk can be called twice without changing values or membership. Its
// visitor returns false to stop, including as soon as the size limit is known.
type replyWalk func(yield func(string) bool)

// replyShape is what a walked reply is to a client, which decides its framing.
// RESP2 sends all but the single value as an array; RESP3 gives a set and a
// map types of their own, and the header of each is sized exactly like the
// rest of the reply.
type replyShape int

const (
	// shapeOne is a single value, or null when the walk yields none: LPOP and
	// SPOP without a count.
	shapeOne replyShape = iota
	shapeArray
	shapeSet
	// shapeMap is keys and values alternating, in the order the walk yields
	// them: HGETALL.
	shapeMap
)

func (f framing) appendShapeHeader(dst []byte, shape replyShape, values int) []byte {
	switch shape {
	case shapeSet:
		return f.appendSetHeader(dst, values)
	case shapeMap:
		return f.appendMapHeader(dst, values/2)
	}
	return appendArrayHeader(dst, values)
}

func (f framing) shapeHeaderSize(shape replyShape, values int) int {
	if shape == shapeMap {
		return f.mapHeaderSize(values / 2)
	}
	return decimalDigits(values) + 3
}

// encodeWalkReply counts exact framing before allocating a single output buffer.
// Map iteration order may differ between passes; payload size remains identical.
func (e *Engine) encodeWalkReply(walk replyWalk, shape replyShape) []byte {
	size, count, fits := 0, 0, true
	walk(func(value string) bool {
		size, fits = addBulkSize(size, len(value))
		count++
		return fits
	})
	if !fits {
		return replyTooLarge
	}
	if shape == shapeOne {
		if count == 0 {
			return e.nullReply()
		}
	} else {
		header := e.shapeHeaderSize(shape, count)
		if size > MaxReplyBytes-header {
			return replyTooLarge
		}
		size += header
	}
	if refusal := e.admitReply(size); refusal != nil {
		return refusal
	}
	out := make([]byte, 0, size)
	if shape != shapeOne {
		out = e.appendShapeHeader(out, shape, count)
	}
	walk(func(value string) bool { out = appendBulkString(out, value); return true })
	return out
}

// hashReply answers a hash's fields, its values, or both - which is HGETALL,
// and a map.
func (e *Engine) hashReply(h *data_structure.Hash, fields, values bool) []byte {
	shape := shapeArray
	if fields && values {
		shape = shapeMap
	}
	return e.encodeWalkReply(func(yield func(string) bool) {
		h.Visit(func(field, value string) bool {
			if fields && !yield(field) {
				return false
			}
			return !values || yield(value)
		})
	}, shape)
}

// scoredReply answers sorted-set members, each followed by its score when
// withScores is set, laid out flat as bulk strings: the RESP2 form, and the
// RESP3 one without scores. With scores a RESP3 connection gets scoredReply3.
//
// Callers choose between the two rather than this choosing for them, because
// this has to stay small enough to inline. Inlined, the closure it hands walk
// stays on the stack; called, it escapes, twice per reply, and the
// command-path benchmark holds the RESP2 path to the allocations it had.
func (e *Engine) scoredReply(walk func(func(string, float64) bool), withScores bool) []byte {
	return e.encodeWalkReply(func(yield func(string) bool) {
		walk(func(member string, score float64) bool {
			return yield(member) && (!withScores || yield(formatZScore(score)))
		})
	}, shapeArray)
}

// scoredReply3 answers members with their scores to a RESP3 connection: each
// score a double and, when nested, each member and its score a pair of their
// own - Redis's form for ZRANGE and ZRANGEBYSCORE WITHSCORES and for a ZPOPMIN
// given a count. A ZPOPMIN without one stays flat: [member, score].
func (e *Engine) scoredReply3(walk func(func(string, float64) bool), nested bool) []byte {
	// The closures handed to walk escape, so they capture the framing, not
	// the engine, and leave the engine's escape analysis as it was.
	f := e.framing
	size, pairs, fits := 0, 0, true
	walk(func(member string, score float64) bool {
		pairs++
		if nested {
			if size > MaxReplyBytes-f.pairHeaderSize() {
				fits = false
				return false
			}
			size += f.pairHeaderSize()
		}
		size, fits = addBulkSize(size, len(member))
		if fits {
			size, fits = f.addDoubleSize(size, len(formatZScore(score)))
		}
		return fits
	})
	values := pairs
	if !nested {
		values = 2 * pairs
	}
	header := decimalDigits(values) + 3
	if !fits || size > MaxReplyBytes-header {
		return replyTooLarge
	}
	size += header
	if refusal := e.admitReply(size); refusal != nil {
		return refusal
	}
	out := appendArrayHeader(make([]byte, 0, size), values)
	walk(func(member string, score float64) bool {
		if nested {
			out = f.appendPairHeader(out)
		}
		out = appendBulkString(out, member)
		out = appendDouble(f, out, formatZScore(score))
		return true
	})
	return out
}

// reserveRemoval bounds the canonical SREM/ZREM record and its member-index array
// before destructive pops. The record has a separate 64 MiB ceiling.
func (e *Engine) reserveRemoval(command, key string, count int, walk replyWalk) []byte {
	if count > MaxReplyBytes/16-2 {
		return replyTooLarge
	}
	size := decimalDigits(count+2) + 3
	var fits bool
	size, fits = addBulkSize(size, len(command))
	if !fits {
		return replyTooLarge
	}
	size, fits = addBulkSize(size, len(key))
	if !fits {
		return replyTooLarge
	}
	walk(func(value string) bool { size, fits = addBulkSize(size, len(value)); return fits })
	// The removal member array and canonical log are constructed only after
	// output admission succeeds. Reserve both now, before membership changes.
	if !fits {
		return replyTooLarge
	}
	logCharge := 0
	if e.aof.file != nil && !e.aof.replaying {
		// Appending can replace a partially filled backing array. Charge the
		// whole future log, including growth overlap, rather than just the new
		// record. The original buffer is already in the transport's base charge.
		maxInt := int(^uint(0) >> 1)
		if len(e.aof.buf) > maxInt/3-size-8192 {
			return allocationPressure
		}
		logCharge = 3 * (len(e.aof.buf) + size + 8192)
	}
	if !e.reserveCommandMemory((count+2)*16 + logCharge + 8192) {
		return allocationPressure
	}
	return nil
}
