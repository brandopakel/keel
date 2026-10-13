package core

import (
	"context"
	"errors"
	"time"
)

// Typed calls: keys and expiry. Each runs the typed operation of the command
// it is named after, in that command's scope (typed_calls.go).

var (
	errExpireNXWithOthers = errors.New("ERR NX and XX, GT or LT options at the same time are not compatible")
	errExpireGTWithLT     = errors.New("ERR GT and LT options at the same time are not compatible")
)

// ExpireCondition is the EXPIRE family's NX, XX, GT and LT: set the expiry
// only if the key has none, only if it has one, only if the new one is
// later, or earlier.
type ExpireCondition struct {
	NX, XX, GT, LT bool
}

// check is the EXPIRE family's refusal of conditions that exclude each
// other, in Redis's words.
func (c ExpireCondition) check() error {
	switch {
	case c.NX && (c.XX || c.GT || c.LT):
		return errExpireNXWithOthers
	case c.GT && c.LT:
		return errExpireGTWithLT
	}
	return nil
}

// Del deletes each of keys from whichever keyspace holds it and answers how
// many it deleted: DEL.
func (e *Engine) Del(ctx context.Context, keys ...string) (n int64, err error) {
	err = e.call(ctx, "DEL", keys, func() error {
		n = e.opDel(keys)
		return nil
	})
	return n, err
}

// Exists counts the keys that exist, of any type, a key named twice counted
// twice: EXISTS.
func (e *Engine) Exists(ctx context.Context, keys ...string) (n int64, err error) {
	err = e.call(ctx, "EXISTS", keys, func() error {
		n = e.opExists(keys)
		return nil
	})
	return n, err
}

// Type is the name of the type key holds, "none" for none: TYPE.
func (e *Engine) Type(ctx context.Context, key string) (name string, err error) {
	err = e.call(ctx, "TYPE", []string{key}, func() error {
		name = e.opType(key)
		return nil
	})
	return name, err
}

// Rename moves what from holds to to, with its expiry, replacing what to
// held: RENAME; and with nx only if to holds nothing, reporting whether it
// moved it: RENAMENX. A from that holds nothing is RENAME's "no such key".
func (e *Engine) Rename(ctx context.Context, from, to string, nx bool) (renamed bool, err error) {
	name := "RENAME"
	if nx {
		name = "RENAMENX"
	}
	err = e.call(ctx, name, []string{from, to}, func() error {
		renamed, err = e.opRename(from, to, nx)
		return err
	})
	return renamed, err
}

// DBSize counts the keys of every type: DBSIZE.
func (e *Engine) DBSize(ctx context.Context) (n int64, err error) {
	err = e.call(ctx, "DBSIZE", nil, func() error {
		n = e.opDBSize()
		return nil
	})
	return n, err
}

// FlushDB deletes every key: FLUSHDB.
func (e *Engine) FlushDB(ctx context.Context) error {
	return e.call(ctx, "FLUSHDB", nil, func() error {
		e.opFlushDB()
		return nil
	})
}

// ExpireAt sets key's expiry to at, as PEXPIREAT does, under c, and reports
// whether it set it: false for a key that holds nothing, or one c leaves. An
// instant already past deletes the key, as Redis's EXPIRE does.
func (e *Engine) ExpireAt(ctx context.Context, key string, at time.Time, c ExpireCondition) (set bool, err error) {
	if err := c.check(); err != nil {
		return false, err
	}
	ms := at.UnixMilli()
	err = e.call(ctx, "PEXPIREAT", []string{key}, func() error {
		set = e.opExpireAt(key, ms, c.condition())
		return nil
	})
	return set, err
}

// Expire sets key's expiry ttl from now, as PEXPIRE does; see ExpireAt. A
// ttl of zero or less deletes the key, and one whose instant would pass what
// an expiry can hold is PEXPIRE's refusal.
func (e *Engine) Expire(ctx context.Context, key string, ttl time.Duration, c ExpireCondition) (set bool, err error) {
	if err := c.check(); err != nil {
		return false, err
	}
	at := ttl.Milliseconds()
	if at > 0 {
		var ok bool
		if at, ok = expiryInstant(at); !ok {
			return false, invalidExpireTime("PEXPIRE")
		}
	}
	err = e.call(ctx, "PEXPIRE", []string{key}, func() error {
		set = e.opExpireAt(key, at, c.condition())
		return nil
	})
	return set, err
}

// condition is c as the typed operation takes it.
func (c ExpireCondition) condition() expireCondition {
	return expireCondition{nx: c.NX, xx: c.XX, gt: c.GT, lt: c.LT}
}

// Persist removes key's expiry, and reports whether it had one: PERSIST.
func (e *Engine) Persist(ctx context.Context, key string) (removed bool, err error) {
	err = e.call(ctx, "PERSIST", []string{key}, func() error {
		removed = e.opPersist(key)
		return nil
	})
	return removed, err
}

// TTL is how long key has left, in milliseconds, with Redis's two negative
// answers: -2 for a key that holds nothing, -1 for one with no expiry: PTTL.
func (e *Engine) TTL(ctx context.Context, key string) (ms int64, err error) {
	err = e.call(ctx, "PTTL", []string{key}, func() error {
		ms = e.opTTL(key)
		return nil
	})
	return ms, err
}

// ExpireTime is the instant key falls due, in Unix milliseconds, with the
// same two negative answers: PEXPIRETIME.
func (e *Engine) ExpireTime(ctx context.Context, key string) (ms int64, err error) {
	err = e.call(ctx, "PEXPIRETIME", []string{key}, func() error {
		ms = e.opExpireTime(key)
		return nil
	})
	return ms, err
}
