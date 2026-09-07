package core

import (
	"errors"
	"strconv"

	"github.com/brandopakel/keel/internal/constant"
	"github.com/brandopakel/keel/internal/data_structure"
)

// Hash commands.
//
// A hash is the first type here whose emptiness is observable: Redis has no
// empty hash, so removing the last field removes the key. Every command that
// can empty one goes through dropIfEmpty, because a key that exists with no
// fields would answer EXISTS 1 and HGETALL nothing, and would be written into
// the log as an HSET with no pairs - which is a syntax error on replay.

func hashFor(key string) (*data_structure.Hash, bool) {
	return hashStore.Get(key)
}

// dropIfEmpty removes a hash that has no fields left, and reports whether it
// did. The store is told about the size change either way, since a hash that
// shrank still costs less than it did.
func dropIfEmpty(key string, h *data_structure.Hash) bool {
	if h.Len() == 0 {
		hashStore.Delete(key)
		return true
	}
	hashStore.Resize(key)
	return false
}

func cmdHSET(args []string) []byte {
	if len(args) < 3 || len(args)%2 != 1 {
		return Encode(errors.New("ERR wrong number of arguments for 'HSET' command"), false)
	}
	key := args[0]

	h, ok := hashFor(key)
	if !ok {
		h = data_structure.NewHash()
		hashStore.Put(key, h)
	}

	added := 0
	for i := 1; i < len(args); i += 2 {
		if h.Set(args[i], args[i+1]) {
			added++
		}
	}
	// After the writes, so the budget sees what the hash actually costs now.
	hashStore.Resize(key)
	return Encode(added, false)
}

func cmdHSETNX(args []string) []byte {
	if len(args) != 3 {
		return Encode(errors.New("ERR wrong number of arguments for 'HSETNX' command"), false)
	}
	key, field, value := args[0], args[1], args[2]

	h, ok := hashFor(key)
	if ok && h.Exists(field) {
		return constant.RespZero
	}
	if !ok {
		h = data_structure.NewHash()
		hashStore.Put(key, h)
	}
	h.Set(field, value)
	hashStore.Resize(key)
	return constant.RespOne
}

func cmdHGET(args []string) []byte {
	if len(args) != 2 {
		return Encode(errors.New("ERR wrong number of arguments for 'HGET' command"), false)
	}
	h, ok := hashFor(args[0])
	if !ok {
		return constant.RespNil
	}
	value, has := h.Get(args[1])
	if !has {
		return constant.RespNil
	}
	return Encode(value, false)
}

func cmdHMGET(args []string) []byte {
	if len(args) < 2 {
		return Encode(errors.New("ERR wrong number of arguments for 'HMGET' command"), false)
	}
	h, ok := hashFor(args[0])

	out := make([]interface{}, 0, len(args)-1)
	for _, field := range args[1:] {
		if !ok {
			out = append(out, nil)
			continue
		}
		if value, has := h.Get(field); has {
			out = append(out, value)
		} else {
			out = append(out, nil)
		}
	}
	return Encode(out, false)
}

func cmdHDEL(args []string) []byte {
	if len(args) < 2 {
		return Encode(errors.New("ERR wrong number of arguments for 'HDEL' command"), false)
	}
	key := args[0]
	h, ok := hashFor(key)
	if !ok {
		return constant.RespZero
	}

	removed := h.Del(args[1:]...)
	dropIfEmpty(key, h)
	return Encode(removed, false)
}

func cmdHEXISTS(args []string) []byte {
	if len(args) != 2 {
		return Encode(errors.New("ERR wrong number of arguments for 'HEXISTS' command"), false)
	}
	h, ok := hashFor(args[0])
	if !ok || !h.Exists(args[1]) {
		return constant.RespZero
	}
	return constant.RespOne
}

func cmdHLEN(args []string) []byte {
	if len(args) != 1 {
		return Encode(errors.New("ERR wrong number of arguments for 'HLEN' command"), false)
	}
	h, ok := hashFor(args[0])
	if !ok {
		return constant.RespZero
	}
	return Encode(h.Len(), false)
}

func cmdHKEYS(args []string) []byte {
	if len(args) != 1 {
		return Encode(errors.New("ERR wrong number of arguments for 'HKEYS' command"), false)
	}
	h, ok := hashFor(args[0])
	if !ok {
		return constant.RespEmptyArray
	}
	return Encode(h.Fields(), false)
}

func cmdHVALS(args []string) []byte {
	if len(args) != 1 {
		return Encode(errors.New("ERR wrong number of arguments for 'HVALS' command"), false)
	}
	h, ok := hashFor(args[0])
	if !ok {
		return constant.RespEmptyArray
	}
	return Encode(h.Values(), false)
}

// cmdHGETALL answers a flat array of field, value, field, value.
//
// Flat rather than nested because that is what RESP2 clients decode into a map,
// and it is what Redis sends.
func cmdHGETALL(args []string) []byte {
	if len(args) != 1 {
		return Encode(errors.New("ERR wrong number of arguments for 'HGETALL' command"), false)
	}
	h, ok := hashFor(args[0])
	if !ok {
		return constant.RespEmptyArray
	}

	fields, values := h.Entries()
	flat := make([]string, 0, 2*len(fields))
	for i, f := range fields {
		flat = append(flat, f, values[i])
	}
	return Encode(flat, false)
}

// cmdHINCRBY adds to a field, treating a missing key or field as zero.
//
// The reply is the value after the increment, so a client needs no second
// round trip. A field holding something that is not an integer is an error and
// leaves the field alone - it is not reset to the increment.
func cmdHINCRBY(args []string) []byte {
	if len(args) != 3 {
		return Encode(errors.New("ERR wrong number of arguments for 'HINCRBY' command"), false)
	}
	key, field := args[0], args[1]

	delta, valid := counterInteger(args[2])
	if !valid {
		return Encode(errors.New("ERR value is not an integer or out of range"), false)
	}

	// Nothing is created until the increment is known to be valid. A hash put
	// here and then abandoned by an error below would be an empty one, and an
	// empty hash is a key that answers EXISTS 1 and HGETALL nothing, and that a
	// rewrite writes as "HSET key" with no pairs - a syntax error on replay.
	h, existed := hashFor(key)

	current := int64(0)
	if existed {
		if existing, has := h.Get(field); has {
			current, valid = counterInteger(existing)
			if !valid {
				return Encode(errors.New("ERR hash value is not an integer"), false)
			}
		}
	}

	// Overflow wraps in Go and would answer a number the client did not ask
	// for. Redis refuses instead, and so does INCR here.
	if (delta > 0 && current > (1<<63-1)-delta) || (delta < 0 && current < -(1<<63)-delta) {
		return Encode(errors.New("ERR increment or decrement would overflow"), false)
	}

	if !existed {
		h = data_structure.NewHash()
		hashStore.Put(key, h)
	}
	updated := current + delta
	h.Set(field, strconv.FormatInt(updated, 10))
	hashStore.Resize(key)
	return Encode(updated, false)
}
