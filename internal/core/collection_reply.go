package core

import "github.com/brandopakel/keel/internal/data_structure"

// A replyWalk can be called twice without changing values or membership. Its
// visitor returns false to stop, including as soon as the size limit is known.
type replyWalk func(yield func(string) bool)

// encodeWalkReply counts exact framing before allocating a single output buffer.
// Map iteration order may differ between passes; payload size remains identical.
func encodeWalkReply(walk replyWalk, scalar bool) []byte {
	size, count, fits := 0, 0, true
	walk(func(value string) bool {
		size, fits = addBulkSize(size, len(value))
		count++
		return fits
	})
	if !fits {
		return replyTooLarge
	}
	if scalar {
		if count == 0 {
			return []byte("$-1\r\n")
		}
	} else {
		header := decimalDigits(count) + 3
		if size > MaxReplyBytes-header {
			return replyTooLarge
		}
		size += header
	}
	if !reserveReplyMemory(size) {
		return allocationPressure
	}
	out := make([]byte, 0, size)
	if !scalar {
		out = appendArrayHeader(out, count)
	}
	walk(func(value string) bool { out = appendBulkString(out, value); return true })
	return out
}

func hashReply(h *data_structure.Hash, fields, values bool) []byte {
	return encodeWalkReply(func(yield func(string) bool) {
		h.Visit(func(field, value string) bool {
			if fields && !yield(field) {
				return false
			}
			return !values || yield(value)
		})
	}, false)
}

func scoredReply(walk func(func(string, float64) bool), withScores bool) []byte {
	return encodeWalkReply(func(yield func(string) bool) {
		walk(func(member string, score float64) bool {
			return yield(member) && (!withScores || yield(formatZScore(score)))
		})
	}, false)
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
