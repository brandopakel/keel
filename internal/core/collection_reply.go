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

func appendShapeHeader(dst []byte, shape replyShape, values int) []byte {
	switch shape {
	case shapeSet:
		return appendSetHeader(dst, values)
	case shapeMap:
		return appendMapHeader(dst, values/2)
	}
	return appendArrayHeader(dst, values)
}

func shapeHeaderSize(shape replyShape, values int) int {
	if shape == shapeMap {
		return mapHeaderSize(values / 2)
	}
	return decimalDigits(values) + 3
}

// encodeWalkReply counts exact framing before allocating a single output buffer.
// Map iteration order may differ between passes; payload size remains identical.
func encodeWalkReply(walk replyWalk, shape replyShape) []byte {
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
			return nullReply()
		}
	} else {
		header := shapeHeaderSize(shape, count)
		if size > MaxReplyBytes-header {
			return replyTooLarge
		}
		size += header
	}
	if refusal := admitReply(size); refusal != nil {
		return refusal
	}
	out := make([]byte, 0, size)
	if shape != shapeOne {
		out = appendShapeHeader(out, shape, count)
	}
	walk(func(value string) bool { out = appendBulkString(out, value); return true })
	return out
}

// hashReply answers a hash's fields, its values, or both - which is HGETALL,
// and a map.
func hashReply(h *data_structure.Hash, fields, values bool) []byte {
	shape := shapeArray
	if fields && values {
		shape = shapeMap
	}
	return encodeWalkReply(func(yield func(string) bool) {
		h.Visit(func(field, value string) bool {
			if fields && !yield(field) {
				return false
			}
			return !values || yield(value)
		})
	}, shape)
}

// scoredReply answers sorted-set members, each followed by its score when
// withScores is set. RESP2 lays both out flat as bulk strings. RESP3 sends the
// score as a double and, when nested, each member and its score as a pair of
// their own - Redis's form for ZRANGE and ZRANGEBYSCORE WITHSCORES and for a
// ZPOPMIN given a count. A ZPOPMIN without one stays flat: [member, score].
func scoredReply(walk func(func(string, float64) bool), withScores, nested bool) []byte {
	if !withScores || !replyRESP3 {
		return encodeWalkReply(func(yield func(string) bool) {
			walk(func(member string, score float64) bool {
				return yield(member) && (!withScores || yield(formatZScore(score)))
			})
		}, shapeArray)
	}
	size, pairs, fits := 0, 0, true
	walk(func(member string, score float64) bool {
		pairs++
		if nested {
			if size > MaxReplyBytes-pairHeaderSize() {
				fits = false
				return false
			}
			size += pairHeaderSize()
		}
		size, fits = addBulkSize(size, len(member))
		if fits {
			size, fits = addDoubleSize(size, len(formatZScore(score)))
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
	if !reserveReplyMemory(size) {
		return allocationPressure
	}
	out := appendArrayHeader(make([]byte, 0, size), values)
	walk(func(member string, score float64) bool {
		if nested {
			out = appendPairHeader(out)
		}
		out = appendBulkString(out, member)
		out = appendDouble(out, formatZScore(score))
		return true
	})
	return out
}

// reserveRemoval bounds the canonical SREM/ZREM record and its member-index array
// before destructive pops. The record has a separate 64 MiB ceiling.
func reserveRemoval(command, key string, count int, walk replyWalk) []byte {
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
	if aof.file != nil && !aof.replaying {
		// Appending can replace a partially filled backing array. Charge the
		// whole future log, including growth overlap, rather than just the new
		// record. The original buffer is already in the transport's base charge.
		maxInt := int(^uint(0) >> 1)
		if len(aof.buf) > maxInt/3-size-8192 {
			return allocationPressure
		}
		logCharge = 3 * (len(aof.buf) + size + 8192)
	}
	if !reserveCommandMemory((count+2)*16 + logCharge + 8192) {
		return allocationPressure
	}
	return nil
}
