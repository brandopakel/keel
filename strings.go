package keel

import (
	"context"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/brandopakel/keel/internal/core"
)

// SetOption is an option of Set, SetNX and SetXX: an expiry, or keeping the
// one the key has.
type SetOption struct {
	ttl      time.Duration
	at       time.Time
	keepTTL  bool
	relative bool
}

// WithTTL expires the key ttl after it is set, as SET's PX does: to the
// millisecond, and refused unless it is at least one.
func WithTTL(ttl time.Duration) SetOption { return SetOption{ttl: ttl, relative: true} }

// WithExpireAt expires the key at t, as SET's PXAT does.
func WithExpireAt(t time.Time) SetOption { return SetOption{at: t} }

// WithKeepTTL keeps the expiry the key has, as SET's KEEPTTL does, where
// setting it would otherwise clear it.
func WithKeepTTL() SetOption { return SetOption{keepTTL: true} }

// setOptions is opts as the engine takes them, with nx and xx: the last
// expiry given counts, as SET's last does, and an expiry with WithKeepTTL is
// SET's syntax error.
func setOptions(opts []SetOption, nx, xx bool) (core.SetOptions, error) {
	o := core.SetOptions{NX: nx, XX: xx}
	for _, opt := range opts {
		switch {
		case opt.keepTTL:
			o.KeepTTL = true
		case opt.relative:
			if opt.ttl.Milliseconds() <= 0 {
				return o, core.InvalidExpireTime("SET")
			}
			at, ok := core.ExpiryFromTTL(opt.ttl)
			if !ok {
				return o, core.InvalidExpireTime("SET")
			}
			o.ExpireAt = at
		default:
			if opt.at.UnixMilli() <= 0 {
				return o, core.InvalidExpireTime("SET")
			}
			o.ExpireAt = opt.at
		}
	}
	return o, nil
}

// foundAnswer is the answer of a command that reads a string a key may not
// hold: ErrNotFound when it does not.
func foundAnswer(value string, found bool, err error) (string, error) {
	switch {
	case err != nil:
		return "", wrap(err)
	case !found:
		return "", ErrNotFound
	}
	return value, nil
}

// bytesAnswer is a string answer as bytes of its own.
func bytesAnswer(value string, err error) ([]byte, error) {
	if err != nil {
		return nil, err
	}
	return []byte(value), nil
}

// Get is the string key holds: GET. A key that holds nothing is ErrNotFound,
// and one of another type ErrWrongType.
func (db *DB) Get(ctx context.Context, key string) (string, error) {
	return foundAnswer(db.e.Get(ctx, key))
}

// GetBytes is the string key holds, as bytes the caller owns: GET; see Get.
func (db *DB) GetBytes(ctx context.Context, key string) ([]byte, error) {
	return bytesAnswer(db.Get(ctx, key))
}

// Set stores value at key, replacing whatever the key held, of any type, and
// clearing its expiry unless an option sets one or keeps it: SET.
func (db *DB) Set(ctx context.Context, key, value string, opts ...SetOption) error {
	_, err := db.set(ctx, key, value, opts, false, false)
	return err
}

// SetBytes stores value at key, as Set does: SET. The bytes are copied, so
// the caller may reuse them once it returns.
func (db *DB) SetBytes(ctx context.Context, key string, value []byte, opts ...SetOption) error {
	return db.Set(ctx, key, string(value), opts...)
}

// SetNX stores value at key only if the key holds nothing, of any type, and
// reports whether it did: SET with NX.
func (db *DB) SetNX(ctx context.Context, key, value string, opts ...SetOption) (bool, error) {
	return db.set(ctx, key, value, opts, true, false)
}

// SetXX stores value at key only if the key holds something, of any type,
// and reports whether it did: SET with XX.
func (db *DB) SetXX(ctx context.Context, key, value string, opts ...SetOption) (bool, error) {
	return db.set(ctx, key, value, opts, false, true)
}

func (db *DB) set(ctx context.Context, key, value string, opts []SetOption, nx, xx bool) (bool, error) {
	o, err := setOptions(opts, nx, xx)
	if err != nil {
		return false, err
	}
	stored, err := db.e.Set(ctx, key, value, o)
	return stored, wrap(err)
}

// GetDel is the string key held, which it deletes: GETDEL. A key that held
// nothing is ErrNotFound.
func (db *DB) GetDel(ctx context.Context, key string) (string, error) {
	return foundAnswer(db.e.GetDel(ctx, key))
}

// Incr adds one to the integer key holds, a key that holds nothing holding
// 0, and returns the sum: INCR. A value that is not an integer is
// ErrNotInteger, and a sum past the int64 range ErrOverflow; the key's
// expiry is kept.
func (db *DB) Incr(ctx context.Context, key string) (int64, error) {
	n, err := db.e.IncrBy(ctx, "INCR", key, 1)
	return n, wrap(err)
}

// Decr takes one from the integer key holds: DECR; see Incr.
func (db *DB) Decr(ctx context.Context, key string) (int64, error) {
	n, err := db.e.IncrBy(ctx, "DECR", key, -1)
	return n, wrap(err)
}

// IncrBy adds by to the integer key holds: INCRBY; see Incr.
func (db *DB) IncrBy(ctx context.Context, key string, by int64) (int64, error) {
	n, err := db.e.IncrBy(ctx, "INCRBY", key, by)
	return n, wrap(err)
}

// DecrBy takes by from the integer key holds: DECRBY; see Incr.
func (db *DB) DecrBy(ctx context.Context, key string, by int64) (int64, error) {
	n, err := db.e.IncrBy(ctx, "DECRBY", key, by)
	return n, wrap(err)
}

// IncrByFloat adds by to the number key holds, a key that holds nothing
// holding 0, and returns the sum: INCRBYFLOAT, in Redis's precision, a
// long double's, and stored as Redis writes it. A sum that would be infinite
// is ErrOverflow.
func (db *DB) IncrByFloat(ctx context.Context, key string, by float64) (float64, error) {
	return floatAnswer(db.e.IncrByFloat(ctx, key, by))
}

// floatAnswer is INCRBYFLOAT's sum, as Redis writes it, as a float64.
func floatAnswer(sum string, err error) (float64, error) {
	if err != nil {
		return 0, wrap(err)
	}
	return strconv.ParseFloat(sum, 64)
}

// Append adds value to the end of the string key holds, creating it, and
// returns its length after: APPEND.
func (db *DB) Append(ctx context.Context, key, value string) (int64, error) {
	n, err := db.e.Append(ctx, key, value)
	return n, wrap(err)
}

// StrLen is the length of the string key holds, 0 for a key that holds
// nothing: STRLEN.
func (db *DB) StrLen(ctx context.Context, key string) (int64, error) {
	n, err := db.e.StrLen(ctx, key)
	return n, wrap(err)
}

// GetRange is the bytes of the string key holds from start to end, both
// inclusive, counting from its end when negative: GETRANGE.
func (db *DB) GetRange(ctx context.Context, key string, start, end int64) (string, error) {
	s, err := db.e.GetRange(ctx, key, start, end)
	return s, wrap(err)
}

// SetRange overwrites the string key holds from offset with value, padding
// with zero bytes past its end and creating it, and returns its length
// after: SETRANGE.
func (db *DB) SetRange(ctx context.Context, key string, offset int64, value string) (int64, error) {
	n, err := db.e.SetRange(ctx, key, offset, value)
	return n, wrap(err)
}

// MGet is the strings keys hold, by key: MGET, in one look at the instance.
// A key that holds nothing, or holds another type, is not in the map, as
// MGET answers nil for both. With no keys it runs nothing, and the map is
// empty.
func (db *DB) MGet(ctx context.Context, keys ...string) (map[string]string, error) {
	if len(keys) == 0 {
		return map[string]string{}, nil
	}
	values, found, err := db.e.MGet(ctx, keys)
	if err != nil {
		return nil, wrap(err)
	}
	return mgetAnswer(keys, values, found), nil
}

// mgetAnswer is MGET's answer as a map of the keys that hold strings.
func mgetAnswer(keys, values []string, found []bool) map[string]string {
	m := make(map[string]string, len(keys))
	for i, key := range keys {
		if found[i] {
			m[key] = values[i]
		}
	}
	return m
}

// MSet stores each of values at its key, replacing whatever each held, of
// any type, and clearing its expiry: one MSET, so that no call sees some of
// them stored and not the rest. It is logged as that MSET, its keys in
// sorted order. With no values it runs nothing.
func (db *DB) MSet(ctx context.Context, values map[string]string) error {
	if len(values) == 0 {
		return nil
	}
	return wrap(db.e.MSet(ctx, msetPairs(values)))
}

// msetPairs is values as MSET's arguments, keys in sorted order, so that
// the record is the same however the map is ranged over.
func msetPairs(values map[string]string) []string {
	keys := slices.Sorted(maps.Keys(values))
	pairs := make([]string, 0, 2*len(keys))
	for _, key := range keys {
		pairs = append(pairs, key, values[key])
	}
	return pairs
}
