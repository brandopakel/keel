package core

import (
	"context"
	"strconv"
	"time"
)

// Typed calls: strings. Each runs the typed operation of the command it is
// named after, in that command's scope (typed_calls.go); the public package
// gives each its Go signature.

// SetOptions is what a SET asks for beyond storing its value: NX, XX,
// KEEPTTL, or an expiry, as an instant.
type SetOptions struct {
	// NX stores the value only if the key does not exist, XX only if it does.
	NX, XX bool
	// KeepTTL keeps the expiry the key has.
	KeepTTL bool
	// ExpireAt is the instant the key expires, the zero time for none.
	ExpireAt time.Time
}

// Get is the string key holds, and false when it holds none: GET.
func (e *Engine) Get(ctx context.Context, key string) (value string, found bool, err error) {
	err = e.call(ctx, "GET", []string{key}, func() error {
		value, found = e.opGet(key)
		return nil
	})
	return value, found, err
}

// Set stores value at key, replacing whatever the key held, as SET does, and
// reports whether it stored it: NX and XX can decline. An expiry that is not
// a positive instant past the epoch in milliseconds is SET's own refusal.
func (e *Engine) Set(ctx context.Context, key, value string, o SetOptions) (stored bool, err error) {
	req := setRequest{nx: o.NX, xx: o.XX, keepTTL: o.KeepTTL}
	if (o.NX && o.XX) || (o.KeepTTL && !o.ExpireAt.IsZero()) {
		// SET's own refusal of options that exclude each other.
		return false, errSyntax
	}
	if !o.ExpireAt.IsZero() {
		at := o.ExpireAt.UnixMilli()
		if at <= 0 {
			return false, invalidExpireTime("SET")
		}
		req.expires, req.at = true, at
	}
	err = e.call(ctx, "SET", []string{key}, func() error {
		stored = e.opSet(e.setTarget(key), key, value, req)
		return nil
	})
	return stored, err
}

// ExpiryFromTTL is the instant ttl from now, in Redis's milliseconds, as SET
// EX and PX and EXPIRE read a relative time, and false for one past what
// the instant can hold, which those refuse as an invalid expire time.
func ExpiryFromTTL(ttl time.Duration) (time.Time, bool) {
	at, ok := expiryInstant(ttl.Milliseconds())
	if !ok {
		return time.Time{}, false
	}
	return time.UnixMilli(at), true
}

// InvalidExpireTime is the refusal of an expiry that is not positive, in
// the words of the command named.
func InvalidExpireTime(name string) error { return invalidExpireTime(name) }

// GetDel is the string key held, which it deletes, and false when it held
// none: GETDEL.
func (e *Engine) GetDel(ctx context.Context, key string) (value string, found bool, err error) {
	err = e.call(ctx, "GETDEL", []string{key}, func() error {
		value, found = e.opGetDel(key)
		return nil
	})
	return value, found, err
}

// IncrBy adds by to the integer key holds, a missing key holding 0, and
// answers the sum: INCRBY, and INCR, DECR and DECRBY by their own names, so
// that each is logged as the command it is.
func (e *Engine) IncrBy(ctx context.Context, name, key string, by int64) (n int64, err error) {
	err = e.call(ctx, name, []string{key}, func() error {
		switch name {
		case "INCR":
			n, err = e.opIncr(key)
		case "DECR":
			n, err = e.opDecr(key)
		case "DECRBY":
			n, err = e.opDecrBy(key, by)
		default:
			n, err = e.opIncrBy(key, by)
		}
		return err
	})
	return n, err
}

// IncrByFloat adds by, as a client would write it, to the number key holds,
// a missing key holding 0, and answers the sum as Redis writes it:
// INCRBYFLOAT.
func (e *Engine) IncrByFloat(ctx context.Context, key string, by float64) (sum string, err error) {
	increment, ok := parseLongDouble(strconv.FormatFloat(by, 'g', -1, 64))
	if !ok {
		return "", errNotAFloat
	}
	err = e.call(ctx, "INCRBYFLOAT", []string{key}, func() error {
		sum, err = e.opIncrByFloat(key, increment)
		return err
	})
	return sum, err
}

// Append adds value to the end of the string key holds, creating it, and
// answers its length after: APPEND.
func (e *Engine) Append(ctx context.Context, key, value string) (n int64, err error) {
	err = e.call(ctx, "APPEND", []string{key}, func() error {
		n, err = e.opAppend(key, value)
		return err
	})
	return n, err
}

// StrLen is the length of the string key holds, 0 for none: STRLEN.
func (e *Engine) StrLen(ctx context.Context, key string) (n int64, err error) {
	err = e.call(ctx, "STRLEN", []string{key}, func() error {
		n = e.opStrlen(key)
		return nil
	})
	return n, err
}

// GetRange is the part of the string key holds from start to end, both
// inclusive and counting from its end when negative: GETRANGE.
func (e *Engine) GetRange(ctx context.Context, key string, start, end int64) (s string, err error) {
	err = e.call(ctx, "GETRANGE", []string{key}, func() error {
		s = e.opGetRange(key, start, end)
		return nil
	})
	return s, err
}

// SetRange overwrites the string key holds from offset with value, padding
// with zero bytes past its end, and answers its length after: SETRANGE. A
// negative offset is SETRANGE's refusal.
func (e *Engine) SetRange(ctx context.Context, key string, offset int64, value string) (n int64, err error) {
	if offset < 0 {
		return 0, errOffsetRange
	}
	err = e.call(ctx, "SETRANGE", []string{key}, func() error {
		n, err = e.opSetRange(key, offset, value)
		return err
	})
	return n, err
}

// MGet is the string each of keys holds, and whether it holds one: MGET. A
// key of another type reads as none, as MGET answers nil for it, and counts
// as the keyspace hit Redis counts it as (cmdMGET).
func (e *Engine) MGet(ctx context.Context, keys []string) (values []string, found []bool, err error) {
	values, found = make([]string, len(keys)), make([]bool, len(keys))
	err = e.call(ctx, "MGET", keys, func() error {
		for i, key := range keys {
			values[i], found[i] = e.opGet(key)
			held := found[i]
			if !held {
				_, held = e.space.OwnerOf(key)
			}
			e.noteLookup(held)
		}
		return nil
	})
	return values, found, err
}

// MSet stores pairs, keys and values in turn, each value replacing whatever
// its key held, of any type, and clearing its expiry: MSET.
func (e *Engine) MSet(ctx context.Context, pairs []string) error {
	keys := make([]string, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		keys = append(keys, pairs[i])
	}
	return e.call(ctx, "MSET", keys, func() error {
		e.opMSet(pairs)
		return nil
	})
}
