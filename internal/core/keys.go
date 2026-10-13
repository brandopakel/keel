package core

import (
	"time"

	"github.com/brandopakel/keel/internal/data_structure"
)

// Typed operations: keys and expiry (phase 4 of docs/embedding-plan.md; the
// family recipe is docs/phase4-family-recipe.md).
//
// These answer about a name whatever type holds it - DEL, EXISTS, TYPE, the
// expiry commands, DBSIZE, FLUSHDB - so they run with no type check in their
// scope, and look the name up across every keyspace (Space.OwnerOf), which
// reaps a key whose expiry has passed. The reads among them - EXISTS, TYPE,
// TTL and PTTL - count each key they look up as a keyspace hit or a miss, as
// Redis's lookupKeyRead does. Each write stages the records develop
// logged for it, byte for byte: DEL with every key given, PERSIST whether or
// not there was an expiry to remove. Those two log a write that changed
// nothing, which Redis does not (db.c, delGenericCommand, line 1437, counts
// a change only for a key it deleted; expire.c, persistCommand, lines
// 914-919, only for an expiry it removed); the family's missing-commands
// step drops them, as the recipe's rule says.

// opDel deletes each of keys from whichever keyspace holds it, as DEL and
// UNLINK do, and answers how many it deleted. A key named twice is deleted
// once.
func (e *Engine) opDel(keys []string) int64 {
	deleted := int64(0)
	for _, key := range keys {
		if e.space.DeleteAnywhere(key) {
			deleted++
		}
	}
	e.aofRecordArgs("DEL", keys)
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

// opPersist removes key's expiry and reports whether it had one. It is
// recorded as PERSIST either way, as develop logs it (see above).
func (e *Engine) opPersist(key string) bool {
	owner, ok := e.space.OwnerOf(key)
	removed := ok && owner.ClearExpiry(key)
	e.aofRecord("PERSIST", key)
	return removed
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
