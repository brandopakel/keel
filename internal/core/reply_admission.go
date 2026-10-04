package core

import (
	"math/rand"

	"github.com/brandopakel/keel/internal/data_structure"
)

// MaxReplyBytes is the per-client encoded output ceiling. Amplifying reads
// enforce it before allocating their payload, independently of append mode.
const MaxReplyBytes = 64 << 20

var replyTooLarge = []byte("-ERR reply exceeds the 64 MiB output limit\r\n")

func decimalDigits(n int) int {
	digits := 1
	for n >= 10 {
		n /= 10
		digits++
	}
	return digits
}

// addBulkSize checks before adding, including RESP framing, without overflowing.
func addBulkSize(size, length int) (int, bool) {
	framing := decimalDigits(length) + 5
	if length > MaxReplyBytes-size-framing {
		return size, false
	}
	return size + length + framing, true
}

// encodeLookupArray sizes the complete reply before allocating it. Lookup must
// not grow values between passes; command execution is single-threaded. Expiry
// may turn a value into nil, which only reduces the required space. The shape
// is an array or, for SMEMBERS, a set.
func (e *Engine) encodeLookupArray(count int, lookup func(int) (string, bool), shape replyShape) []byte {
	size := e.shapeHeaderSize(shape, count)
	// A null is the smallest element either protocol has.
	if count > (MaxReplyBytes-size)/e.nullSize() {
		return replyTooLarge
	}
	for i := 0; i < count; i++ {
		value, found := lookup(i)
		if !found {
			if size > MaxReplyBytes-e.nullSize() {
				return replyTooLarge
			}
			size += e.nullSize()
			continue
		}
		var fits bool
		size, fits = addBulkSize(size, len(value))
		if !fits {
			return replyTooLarge
		}
	}
	if refusal := e.admitReply(size); refusal != nil {
		return refusal
	}
	out := e.appendShapeHeader(make([]byte, 0, size), shape, count)
	for i := 0; i < count; i++ {
		value, found := lookup(i)
		if found {
			out = appendBulkString(out, value)
		} else {
			out = e.appendNull(out)
		}
	}
	return out
}

// encodeRepeatedMembers fixes the random draw as bounded indexes, then sizes
// its exact payload before allocating it. No member-string array or temporary
// per-member encoded reply is built, and the set's order is unchanged.
func (e *Engine) encodeRepeatedMembers(s *data_structure.Set, count int) []byte {
	if count <= 0 || s.Len() == 0 {
		return appendArrayHeader(nil, 0)
	}
	size := decimalDigits(count) + 3
	// Index storage has its own ceiling as well as the encoded payload ceiling.
	if count > MaxReplyBytes/8 || count > (MaxReplyBytes-size)/6 {
		return replyTooLarge
	}
	if !e.reserveCommandMemory(count*8 + 4096) {
		return allocationPressure
	}
	indices := make([]int, count)
	for i := range indices {
		indices[i] = rand.Intn(s.Len())
		value, _ := s.MemberAt(indices[i])
		var fits bool
		size, fits = addBulkSize(size, len(value))
		if !fits {
			return replyTooLarge
		}
	}
	if refusal := e.admitReply(size); refusal != nil {
		return refusal
	}
	out := appendArrayHeader(make([]byte, 0, size), count)
	for _, index := range indices {
		value, _ := s.MemberAt(index)
		out = appendBulkString(out, value)
	}
	return out
}
