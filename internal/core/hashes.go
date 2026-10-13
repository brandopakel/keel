package core

import (
	"errors"
	"math"
	"strconv"

	"github.com/brandopakel/keel/internal/data_structure"
)

// Typed operations: hashes (phase 4 of docs/embedding-plan.md; the family
// recipe is docs/phase4-family-recipe.md).
//
// A hash is the first type whose emptiness is observable: Redis has no empty
// hash, so removing the last field removes the key (dropIfEmpty), and a hash
// is created only once something will be put in it. Each write stages the
// record develop logged for it, byte for byte: HSET, HSETNX, HDEL and HINCRBY
// with their arguments as given. HSETNX of a field that exists and HDEL that
// removes nothing are logged although they change nothing, which Redis does
// not (t_hash.c counts a change only when there was one); the family's
// missing-commands step drops them, as the recipe's rule says.

// errHashNotInteger refuses HINCRBY of a field that does not hold an integer,
// in Redis's words.
var errHashNotInteger = errors.New("ERR hash value is not an integer")

// hashFor is the hash key holds, and false for none.
func (e *Engine) hashFor(key string) (*data_structure.Hash, bool) {
	return e.hashStore.Get(key)
}

// hashForWrite is the hash key holds, created if there is none.
func (e *Engine) hashForWrite(key string) *data_structure.Hash {
	h, ok := e.hashFor(key)
	if !ok {
		h = data_structure.NewHash()
		e.hashStore.Put(key, h)
	}
	return h
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

// opHSet sets each field of pairs, field value field value, in the hash key
// holds, creating it, and answers how many fields it added rather than
// replaced. It is recorded as the HSET it is.
func (e *Engine) opHSet(key string, pairs []string) int64 {
	h := e.hashForWrite(key)
	added := int64(0)
	for i := 0; i < len(pairs); i += 2 {
		if h.Set(pairs[i], pairs[i+1]) {
			added++
		}
	}
	// After the writes, so the budget sees what the hash actually costs now.
	e.hashStore.Resize(key)
	e.aofRecordKeyArgs("HSET", key, pairs)
	return added
}

// opHSetNX sets field to value only if the hash key holds has no such field,
// creating the hash, and reports whether it did. It is recorded as HSETNX
// either way, as develop logs it (see above).
func (e *Engine) opHSetNX(key, field, value string) bool {
	e.aofRecord("HSETNX", key, field, value)
	h, ok := e.hashFor(key)
	if ok && h.Exists(field) {
		return false
	}
	if !ok {
		h = data_structure.NewHash()
		e.hashStore.Put(key, h)
	}
	h.Set(field, value)
	e.hashStore.Resize(key)
	return true
}

// opHGet is the value of field in the hash key holds, and false for none.
func (e *Engine) opHGet(key, field string) (string, bool) {
	h, ok := e.hashFor(key)
	if !ok {
		return "", false
	}
	return h.Get(field)
}

// opHDel removes each of fields from the hash key holds, removing the key
// with its last field, and answers how many it removed. It is recorded as
// HDEL whatever it removed, as develop logs it (see above).
func (e *Engine) opHDel(key string, fields []string) int64 {
	e.aofRecordKeyArgs("HDEL", key, fields)
	h, ok := e.hashFor(key)
	if !ok {
		return 0
	}
	removed := h.Del(fields...)
	e.dropIfEmpty(key, h)
	return int64(removed)
}

// opHExists reports whether the hash key holds has field.
func (e *Engine) opHExists(key, field string) bool {
	h, ok := e.hashFor(key)
	return ok && h.Exists(field)
}

// opHLen counts the fields of the hash key holds.
func (e *Engine) opHLen(key string) int64 {
	h, ok := e.hashFor(key)
	if !ok {
		return 0
	}
	return int64(h.Len())
}

// opHIncrBy adds delta to the integer field holds in the hash key holds, a
// missing key or field being 0, and answers the sum. A field that does not
// hold an integer is refused and left alone, and so is a sum that would
// overflow; nothing is created until the sum is known to be good, since a
// hash put here and then abandoned would be an empty one. It is recorded as
// HINCRBY with the increment in decimal, the text a client's canonical
// integer has.
func (e *Engine) opHIncrBy(key, field string, delta int64) (int64, error) {
	h, existed := e.hashFor(key)
	current := int64(0)
	if existed {
		if existing, has := h.Get(field); has {
			var valid bool
			if current, valid = e.counterInteger(existing); !valid {
				return 0, errHashNotInteger
			}
		}
	}
	// Overflow wraps in Go and would answer a number the client did not ask
	// for. Redis refuses instead, and so does INCR here.
	if (delta > 0 && current > math.MaxInt64-delta) || (delta < 0 && current < math.MinInt64-delta) {
		return 0, errIncrOverflow
	}
	if !existed {
		h = data_structure.NewHash()
		e.hashStore.Put(key, h)
	}
	updated := current + delta
	h.Set(field, strconv.FormatInt(updated, 10))
	e.hashStore.Resize(key)
	e.aofRecord("HINCRBY", key, field, strconv.FormatInt(delta, 10))
	return updated, nil
}
