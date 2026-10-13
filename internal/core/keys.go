package core

import (
	"errors"
	"time"

	"github.com/brandopakel/keel/internal/data_structure"
)

// Typed operations: keys and expiry (phase 4 of docs/embedding-plan.md; the
// family recipe is docs/phase4-family-recipe.md).
//
// These answer about a name whatever type holds it - DEL, EXISTS, TYPE, the
// expiry commands, DBSIZE, FLUSHDB, TOUCH, RENAME - so they run with no type
// check in their scope, and look the name up across every keyspace
// (Space.OwnerOf), which reaps a key whose expiry has passed. The reads among
// them - EXISTS, TYPE, TTL and PTTL, EXPIRETIME and PEXPIRETIME, TOUCH, and
// COPY's source - count each key they look up as a keyspace hit or a miss,
// as Redis's lookupKeyRead does. A write is recorded only when it changed
// something, as Redis propagates a command only when it counted a change
// (server.dirty).

var errNoSuchKey = errors.New("ERR no such key")

// opDel deletes each of keys from whichever keyspace holds it, as DEL and
// UNLINK do, and answers how many it deleted. A key named twice is deleted
// once. It is recorded as DEL with every key given, as Redis propagates the
// command, and not at all when it deleted nothing (db.c, delGenericCommand,
// counts a change only for a key it deleted).
func (e *Engine) opDel(keys []string) int64 {
	deleted := int64(0)
	for _, key := range keys {
		if e.space.DeleteAnywhere(key) {
			deleted++
		}
	}
	if deleted > 0 {
		e.aofRecordArgs("DEL", keys)
	}
	return deleted
}

// opExists counts the keys that exist, of any type, a key named twice
// counted twice, as Redis counts it.
func (e *Engine) opExists(keys []string) int64 {
	found := int64(0)
	for _, key := range keys {
		_, held := e.space.OwnerOf(key)
		e.noteLookup(held)
		if held {
			found++
		}
	}
	return found
}

// opType is the name of the keyspace that holds key, and "none" for none.
func (e *Engine) opType(key string) string {
	owner, held := e.space.OwnerOf(key)
	e.noteLookup(held)
	if !held {
		return "none"
	}
	return owner.KeyspaceName()
}

// opTTL is how long key has left, in milliseconds. The two negative answers
// are Redis's: -2 for a key that is not there, -1 for one with no expiry.
func (e *Engine) opTTL(key string) int64 {
	owner, ok := e.space.OwnerOf(key)
	e.noteLookup(ok)
	if !ok {
		return -2
	}
	at, has := owner.GetExpiry(key)
	if !has {
		return -1
	}
	return max(0, int64(at)-time.Now().UnixMilli())
}

// expireCondition is EXPIRE's NX, XX, GT and LT: set the expiry only if the
// key has none, has one, would get a later one, or an earlier one. A key
// with no expiry has an infinite one, so GT never sets it and LT always
// does, as in Redis.
type expireCondition struct {
	nx, xx, gt, lt bool
}

// opExpireAt sets key's expiry at at, in Unix milliseconds, as the EXPIRE
// family does, and reports whether it did. A key that is not there, or whose
// expiry the condition keeps, is left, and nothing is recorded. An instant
// at or before now, the clock reading the command took once (e.commandClock),
// deletes the key, recorded as the DEL it is, except while the log replays or
// a replica applies, which set the expiry as given, as Redis's
// checkAlreadyExpired leaves it to the expiry to do; otherwise the expiry is
// recorded as PEXPIREAT. Judging against the instant a relative expiry was
// computed from is Redis's commandTimeSnapshot() (expire.c 652-661).
func (e *Engine) opExpireAt(key string, at int64, c expireCondition, now int64) bool {
	owner, ok := e.space.OwnerOf(key)
	if !ok {
		return false
	}
	old, has := owner.GetExpiry(key)
	if (c.nx && has) || (c.xx && !has) || (c.gt && (!has || at <= int64(old))) || (c.lt && has && at >= int64(old)) {
		return false
	}
	if at <= now && !e.aof.replaying && !e.replicaApplying {
		owner.Delete(key)
		e.aofRecord("DEL", key)
		return true
	}
	owner.SetExpiryAt(key, uint64(at))
	e.aofRecordInt("PEXPIREAT", key, at)
	return true
}

// opPersist removes key's expiry and reports whether it had one, recorded
// as PERSIST only then (expire.c, persistCommand, lines 914-919).
func (e *Engine) opPersist(key string) bool {
	owner, ok := e.space.OwnerOf(key)
	removed := ok && owner.ClearExpiry(key)
	if removed {
		e.aofRecord("PERSIST", key)
	}
	return removed
}

// opExpireTime is the instant key falls due, in Unix milliseconds, as
// EXPIRETIME and PEXPIRETIME answer it: -2 for a key that is not there, -1
// for one with no expiry.
func (e *Engine) opExpireTime(key string) int64 {
	owner, ok := e.space.OwnerOf(key)
	e.noteLookup(ok)
	if !ok {
		return -2
	}
	at, has := owner.GetExpiry(key)
	if !has {
		return -1
	}
	return int64(at)
}

// opTouch records a use of each of keys, as a read does, and counts the keys
// that exist, a key named twice counted twice: TOUCH.
func (e *Engine) opTouch(keys []string) int64 {
	touched := int64(0)
	for _, key := range keys {
		owner, held := e.space.OwnerOf(key)
		e.noteLookup(held)
		if held && owner.Touch(key) {
			touched++
		}
	}
	return touched
}

// opRename moves the value from holds to the name to, with its expiry, as
// RENAME does, replacing whatever to held, of any type; with nx it leaves a
// to that exists, and reports false. A from that is not there is refused.
// Renaming a key to itself changes nothing. The value is moved, not copied,
// so a rename costs the same whatever the value's size. It is recorded as
// RENAME, which replays to the same state (a log holding it is not read by a
// build before this one), and not at all when it changed nothing.
func (e *Engine) opRename(from, to string, nx bool) (bool, error) {
	owner, held := e.space.OwnerOf(from)
	if !held {
		return false, errNoSuchKey
	}
	if from == to {
		return !nx, nil
	}
	if _, exists := e.space.OwnerOf(to); exists {
		if nx {
			return false, nil
		}
		e.space.DeleteAnywhere(to)
	}
	owner.Rename(from, to)
	e.aofRecord("RENAME", from, to)
	return true, nil
}

// opDBSize counts the keys of every keyspace.
func (e *Engine) opDBSize() int64 { return int64(e.space.TotalKeys()) }

// opFlushDB deletes every key of every keyspace, recorded as the plain
// FLUSHDB every release replays. Each key is marked for a rewrite walking
// the keyspace and for the replicas, before it is deleted: a rewrite that
// has written some of them into the new log would otherwise finish by
// restoring what FLUSHDB removed.
func (e *Engine) opFlushDB() {
	e.aofRecord("FLUSHDB")
	e.space.EachKeyspace(func(ks data_structure.Keyspace) {
		for _, key := range ks.Keys() {
			e.noteRewriteDirty(key)
			e.noteReplicationDirty(key)
			ks.Delete(key)
		}
	})
}
