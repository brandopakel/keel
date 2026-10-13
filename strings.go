package keel

import (
	"context"
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

// Get is the string key holds: GET. A key that holds nothing is ErrNotFound,
// and one of another type ErrWrongType.
func (db *DB) Get(ctx context.Context, key string) (string, error) {
	value, found, err := db.e.Get(ctx, key)
	switch {
	case err != nil:
		return "", wrap(err)
	case !found:
		return "", ErrNotFound
	}
	return value, nil
}

// Set stores value at key, replacing whatever the key held, of any type, and
// clearing its expiry unless an option sets one or keeps it: SET.
func (db *DB) Set(ctx context.Context, key, value string, opts ...SetOption) error {
	_, err := db.set(ctx, key, value, opts, false, false)
	return err
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
	value, found, err := db.e.GetDel(ctx, key)
	switch {
	case err != nil:
		return "", wrap(err)
	case !found:
		return "", ErrNotFound
	}
	return value, nil
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
	sum, err := db.e.IncrByFloat(ctx, key, by)
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
