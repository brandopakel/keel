package core

import (
	"errors"
	"math"
	"math/big"
	"slices"
	"strconv"

	"github.com/brandopakel/keel/internal/data_structure"
)

// Typed operations: hashes (phase 4 of docs/embedding-plan.md; the family
// recipe is docs/phase4-family-recipe.md).
//
// A hash is the first type whose emptiness is observable: Redis has no empty
// hash, so removing the last field removes the key (dropIfEmpty), and a hash
// is created only once something will be put in it. A write is recorded only
// when it changed something, as t_hash.c counts a change (server.dirty): HSET
// and HMSET as HSET with their pairs, HSETNX when it set the field, HDEL and
// HGETDEL as HDEL with every field given when they removed one, HINCRBY with
// its increment, and HINCRBYFLOAT as an HSET of the value it made.

// errHashNotInteger refuses HINCRBY of a field that does not hold an integer,
// in Redis's words.
var (
	errHashNotInteger = errors.New("ERR hash value is not an integer")
	errHashNotFloat   = errors.New("ERR hash value is not a float")
	errNaNOrInfinity  = errors.New("ERR value is NaN or Infinity")

	errHGetDelFields      = errors.New("ERR Mandatory argument FIELDS is missing or not at the right position")
	errHGetDelCount       = errors.New("ERR Number of fields must be a positive integer")
	errHGetDelCountsMatch = errors.New("ERR The `numfields` parameter must match the number of arguments")
)

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
// creating the hash, and reports whether it did; it is recorded as HSETNX
// only then (t_hash.c, hsetnxCommand, returns before server.dirty for a
// field that exists).
func (e *Engine) opHSetNX(key, field, value string) bool {
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
	e.aofRecord("HSETNX", key, field, value)
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
// HDEL with every field given when it removed any, as Redis propagates it,
// and not at all otherwise (t_hash.c, hdelCommand, counts a change only for
// a field it deleted).
func (e *Engine) opHDel(key string, fields []string) int64 {
	h, ok := e.hashFor(key)
	if !ok {
		return 0
	}
	removed := h.Del(fields...)
	e.dropIfEmpty(key, h)
	if removed > 0 {
		e.aofRecordKeyArgs("HDEL", key, fields)
	}
	return int64(removed)
}

// opHStrlen is the length of the value of field in the hash key holds, 0
// for none.
func (e *Engine) opHStrlen(key, field string) int64 {
	value, _ := e.opHGet(key, field)
	return int64(len(value))
}

// hashFloatSum is the text HINCRBYFLOAT would store in field of the hash key
// holds, adding incr in Redis's long double (long_double.go), with nothing
// written yet, so that the command can admit its reply first.
func (e *Engine) hashFloatSum(key, field string, incr *big.Float) (string, error) {
	current := new(big.Float).SetPrec(longDoublePrec)
	if h, ok := e.hashFor(key); ok {
		if value, has := h.Get(field); has {
			var valid bool
			if current, valid = parseLongDouble(value); !valid {
				return "", errHashNotFloat
			}
		}
	}
	sum, finite := addLongDouble(current, incr)
	if !finite {
		return "", errFloatInfinite
	}
	return formatLongDoubleHuman(sum), nil
}

// storeHashField sets field to value in the hash key holds, creating it,
// recorded as the HSET it is: HINCRBYFLOAT's record, as Redis propagates the
// final value rather than the increment.
func (e *Engine) storeHashField(key, field, value string) {
	h := e.hashForWrite(key)
	h.Set(field, value)
	e.hashStore.Resize(key)
	e.aofRecord("HSET", key, field, value)
}

// opHIncrByFloat adds incr to the number field holds in the hash key holds,
// a missing key or field being 0, and answers the sum as it stores it.
func (e *Engine) opHIncrByFloat(key, field string, incr *big.Float) (string, error) {
	value, err := e.hashFloatSum(key, field, incr)
	if err == nil {
		e.storeHashField(key, field, value)
	}
	return value, err
}

// hashFieldsGotten is what HGETDEL finds of fields in the hash key holds, in
// order, each found field taken by its first mention: a field named twice is
// found once, and the second mention finds nothing, as Redis deletes the
// field before it looks at the next.
func (e *Engine) hashFieldsGotten(key string, fields []string) (values []string, found []bool) {
	values, found = make([]string, len(fields)), make([]bool, len(fields))
	h, ok := e.hashFor(key)
	if !ok {
		return values, found
	}
	var seen map[string]bool
	if len(fields) > 8 {
		seen = make(map[string]bool, len(fields))
	}
	for i, field := range fields {
		repeated := false
		if seen != nil {
			repeated, seen[field] = seen[field], true
		} else {
			repeated = slices.Contains(fields[:i], field)
		}
		if !repeated {
			values[i], found[i] = h.Get(field)
		}
	}
	return values, found
}

// opHGetDel answers the values of fields in the hash key holds, as
// hashFieldsGotten finds them, and deletes those found, the key with its
// last field. It is recorded as HDEL with every field given when it removed
// any, as Redis propagates HGETDEL.
func (e *Engine) opHGetDel(key string, fields []string) ([]string, []bool) {
	values, found := e.hashFieldsGotten(key, fields)
	e.deleteGottenFields(key, fields, found)
	return values, found
}

// deleteGottenFields deletes the fields hashFieldsGotten found.
func (e *Engine) deleteGottenFields(key string, fields []string, found []bool) {
	if !slices.Contains(found, true) {
		return
	}
	h, _ := e.hashFor(key)
	for i, field := range fields {
		if found[i] {
			h.Del(field)
		}
	}
	e.dropIfEmpty(key, h)
	e.aofRecordKeyArgs("HDEL", key, fields)
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
