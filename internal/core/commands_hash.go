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

func (e *Engine) hashFor(key string) (*data_structure.Hash, bool) {
	return e.hashStore.Get(key)
}

// dropIfEmpty removes a hash that has no fields left, and reports whether it
// did. The store is told about the size change either way, since a hash that
// shrank still costs less than it did.
func (e *Engine) dropIfEmpty(key string, h *data_structure.Hash) bool {
	if h.Len() == 0 {
		e.hashStore.Delete(key)
		return true
	}
	e.hashStore.Resize(key)
	return false
}

func (e *Engine) cmdHSET(args []string) []byte {
	if len(args) < 3 || len(args)%2 != 1 {
		return e.encode(wrongArguments("HSET"), false)
	}
	key := args[0]

	h, ok := e.hashFor(key)
	if !ok {
		h = data_structure.NewHash()
		e.hashStore.Put(key, h)
	}

	added := 0
	for i := 1; i < len(args); i += 2 {
		if h.Set(args[i], args[i+1]) {
			added++
		}
	}
	// After the writes, so the budget sees what the hash actually costs now.
	e.hashStore.Resize(key)
	return e.encode(added, false)
}

func (e *Engine) cmdHSETNX(args []string) []byte {
	if len(args) != 3 {
		return e.encode(wrongArguments("HSETNX"), false)
	}
	key, field, value := args[0], args[1], args[2]

	h, ok := e.hashFor(key)
	if ok && h.Exists(field) {
		return constant.RespZero
	}
	if !ok {
		h = data_structure.NewHash()
		e.hashStore.Put(key, h)
	}
	h.Set(field, value)
	e.hashStore.Resize(key)
	return constant.RespOne
}

func (e *Engine) cmdHGET(args []string) []byte {
	if len(args) != 2 {
		return e.encode(wrongArguments("HGET"), false)
	}
	h, ok := e.hashFor(args[0])
	if !ok {
		return e.nullReply()
	}
	value, has := h.Get(args[1])
	if !has {
		return e.nullReply()
	}
	return e.encodeBoundedString(value)
}

func (e *Engine) cmdHMGET(args []string) []byte {
	if len(args) < 2 {
		return e.encode(wrongArguments("HMGET"), false)
	}
	h, ok := e.hashFor(args[0])

	return e.encodeLookupArray(len(args)-1, func(i int) (string, bool) {
		if !ok {
			return "", false
		}
		return h.Get(args[i+1])
	}, shapeArray)
}

func (e *Engine) cmdHDEL(args []string) []byte {
	if len(args) < 2 {
		return e.encode(wrongArguments("HDEL"), false)
	}
	key := args[0]
	h, ok := e.hashFor(key)
	if !ok {
		return constant.RespZero
	}

	removed := h.Del(args[1:]...)
	e.dropIfEmpty(key, h)
	return e.encode(removed, false)
}

func (e *Engine) cmdHEXISTS(args []string) []byte {
	if len(args) != 2 {
		return e.encode(wrongArguments("HEXISTS"), false)
	}
	h, ok := e.hashFor(args[0])
	if !ok || !h.Exists(args[1]) {
		return constant.RespZero
	}
	return constant.RespOne
}

func (e *Engine) cmdHLEN(args []string) []byte {
	if len(args) != 1 {
		return e.encode(wrongArguments("HLEN"), false)
	}
	h, ok := e.hashFor(args[0])
	if !ok {
		return constant.RespZero
	}
	return e.encode(h.Len(), false)
}

func (e *Engine) cmdHKEYS(args []string) []byte {
	if len(args) != 1 {
		return e.encode(wrongArguments("HKEYS"), false)
	}
	h, ok := e.hashFor(args[0])
	if !ok {
		return constant.RespEmptyArray
	}
	return e.hashReply(h, true, false)
}

func (e *Engine) cmdHVALS(args []string) []byte {
	if len(args) != 1 {
		return e.encode(wrongArguments("HVALS"), false)
	}
	h, ok := e.hashFor(args[0])
	if !ok {
		return constant.RespEmptyArray
	}
	return e.hashReply(h, false, true)
}

// cmdHGETALL answers a map of field to value: in RESP2 a flat array of field,
// value, field, value.
//
// Flat rather than nested because that is what RESP2 clients decode into a map,
// and it is what Redis sends.
func (e *Engine) cmdHGETALL(args []string) []byte {
	if len(args) != 1 {
		return e.encode(wrongArguments("HGETALL"), false)
	}
	h, ok := e.hashFor(args[0])
	if !ok {
		return e.emptyMapReply()
	}

	return e.hashReply(h, true, true)
}

// cmdHINCRBY adds to a field, treating a missing key or field as zero.
//
// The reply is the value after the increment, so a client needs no second
// round trip. A field holding something that is not an integer is an error and
// leaves the field alone - it is not reset to the increment.
func (e *Engine) cmdHINCRBY(args []string) []byte {
	if len(args) != 3 {
		return e.encode(wrongArguments("HINCRBY"), false)
	}
	key, field := args[0], args[1]

	delta, valid := e.counterInteger(args[2])
	if !valid {
		return e.encode(errors.New("ERR value is not an integer or out of range"), false)
	}

	// Nothing is created until the increment is known to be valid. A hash put
	// here and then abandoned by an error below would be an empty one, and an
	// empty hash is a key that answers EXISTS 1 and HGETALL nothing, and that a
	// rewrite writes as "HSET key" with no pairs - a syntax error on replay.
	h, existed := e.hashFor(key)

	current := int64(0)
	if existed {
		if existing, has := h.Get(field); has {
			current, valid = e.counterInteger(existing)
			if !valid {
				return e.encode(errors.New("ERR hash value is not an integer"), false)
			}
		}
	}

	// Overflow wraps in Go and would answer a number the client did not ask
	// for. Redis refuses instead, and so does INCR here.
	if (delta > 0 && current > (1<<63-1)-delta) || (delta < 0 && current < -(1<<63)-delta) {
		return e.encode(errors.New("ERR increment or decrement would overflow"), false)
	}

	if !existed {
		h = data_structure.NewHash()
		e.hashStore.Put(key, h)
	}
	updated := current + delta
	h.Set(field, strconv.FormatInt(updated, 10))
	e.hashStore.Resize(key)
	return e.encode(updated, false)
}
