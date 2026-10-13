package keel

import (
	"context"
	"time"

	"github.com/brandopakel/keel/internal/core"
)

// Del deletes each of keys, of any type, and returns how many it deleted:
// DEL.
func (db *DB) Del(ctx context.Context, keys ...string) (int64, error) {
	n, err := db.e.Del(ctx, keys...)
	return n, wrap(err)
}

// Exists counts the keys that hold something, a key named twice counted
// twice: EXISTS.
func (db *DB) Exists(ctx context.Context, keys ...string) (int64, error) {
	n, err := db.e.Exists(ctx, keys...)
	return n, wrap(err)
}

// Type is the name of the type key holds - "string", "hash", "list", "set",
// "zset", and the others - and "none" for a key that holds nothing: TYPE.
func (db *DB) Type(ctx context.Context, key string) (string, error) {
	name, err := db.e.Type(ctx, key)
	return name, wrap(err)
}

// Rename moves what from holds to to, with its expiry, replacing whatever to
// held: RENAME. A from that holds nothing is Redis's "no such key".
func (db *DB) Rename(ctx context.Context, from, to string) error {
	_, err := db.e.Rename(ctx, from, to, false)
	return wrap(err)
}

// RenameNX moves what from holds to to only if to holds nothing, and reports
// whether it did: RENAMENX.
func (db *DB) RenameNX(ctx context.Context, from, to string) (bool, error) {
	renamed, err := db.e.Rename(ctx, from, to, true)
	return renamed, wrap(err)
}

// DBSize counts the keys: DBSIZE.
func (db *DB) DBSize(ctx context.Context) (int64, error) {
	n, err := db.e.DBSize(ctx)
	return n, wrap(err)
}

// FlushDB deletes every key: FLUSHDB.
func (db *DB) FlushDB(ctx context.Context) error {
	return wrap(db.e.FlushDB(ctx))
}

// NoTTL is the TTL of a key that does not expire.
const NoTTL time.Duration = -1

// ExpireOption is a condition of Expire and ExpireAt, as the EXPIRE family's.
type ExpireOption uint8

const (
	// ExpireNX sets the expiry only if the key has none: NX.
	ExpireNX ExpireOption = iota + 1
	// ExpireXX sets it only if the key has one: XX.
	ExpireXX
	// ExpireGT sets it only if it is later than the key's: GT, a key with no
	// expiry having an infinite one.
	ExpireGT
	// ExpireLT sets it only if it is earlier than the key's: LT.
	ExpireLT
)

func expireCondition(opts []ExpireOption) core.ExpireCondition {
	var c core.ExpireCondition
	for _, opt := range opts {
		switch opt {
		case ExpireNX:
			c.NX = true
		case ExpireXX:
			c.XX = true
		case ExpireGT:
			c.GT = true
		case ExpireLT:
			c.LT = true
		}
	}
	return c
}

// Expire sets key to expire ttl from now, to the millisecond, and reports
// whether it did: false for a key that holds nothing, or one the condition
// leaves. A ttl of zero or less deletes the key, as Redis's PEXPIRE does.
func (db *DB) Expire(ctx context.Context, key string, ttl time.Duration, opts ...ExpireOption) (bool, error) {
	set, err := db.e.Expire(ctx, key, ttl, expireCondition(opts))
	return set, wrap(err)
}

// ExpireAt sets key to expire at t, as PEXPIREAT does; see Expire. An
// instant already past deletes the key.
func (db *DB) ExpireAt(ctx context.Context, key string, t time.Time, opts ...ExpireOption) (bool, error) {
	set, err := db.e.ExpireAt(ctx, key, t, expireCondition(opts))
	return set, wrap(err)
}

// Persist removes key's expiry, and reports whether it had one: PERSIST.
func (db *DB) Persist(ctx context.Context, key string) (bool, error) {
	removed, err := db.e.Persist(ctx, key)
	return removed, wrap(err)
}

// TTL is how long key has left, to the millisecond: PTTL. A key that holds
// nothing is ErrNotFound, and one that does not expire answers NoTTL.
func (db *DB) TTL(ctx context.Context, key string) (time.Duration, error) {
	return ttlAnswer(db.e.TTL(ctx, key))
}

// ttlAnswer is PTTL's answer as a duration.
func ttlAnswer(ms int64, err error) (time.Duration, error) {
	switch {
	case err != nil:
		return 0, wrap(err)
	case ms == -2:
		return 0, ErrNotFound
	case ms == -1:
		return NoTTL, nil
	}
	return time.Duration(ms) * time.Millisecond, nil
}

// ExpireTime is the instant key expires, to the millisecond: PEXPIRETIME. A
// key that holds nothing is ErrNotFound, and one that does not expire answers
// the zero time.
func (db *DB) ExpireTime(ctx context.Context, key string) (time.Time, error) {
	return expireTimeAnswer(db.e.ExpireTime(ctx, key))
}

// expireTimeAnswer is PEXPIRETIME's answer as an instant.
func expireTimeAnswer(ms int64, err error) (time.Time, error) {
	switch {
	case err != nil:
		return time.Time{}, wrap(err)
	case ms == -2:
		return time.Time{}, ErrNotFound
	case ms == -1:
		return time.Time{}, nil
	}
	return time.UnixMilli(ms), nil
}
