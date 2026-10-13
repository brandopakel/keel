package core

import (
	"context"
	"time"
)

// Typed calls the public package builds its Keel-native methods on: a
// compare-and-swap on SET's IFEQ, a conditional delete on DELEX's, and a
// snapshot of the counters INFO renders.

// CompareAndSwap stores value at key only if key holds the string old, as SET
// key value IFEQ old does, and reports whether it stored it: a key that holds
// nothing, or another string, is left, and one of another type is
// WRONGTYPE. It is logged as the SET it performs, with its expiry.
func (e *Engine) CompareAndSwap(ctx context.Context, key, old, value string, o SetOptions) (swapped bool, err error) {
	req := setRequest{keepTTL: o.KeepTTL}
	if o.KeepTTL && !o.ExpireAt.IsZero() {
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
		t := e.setTarget(key)
		holds, err := e.setConditionHolds(t, valueCondition{kind: condEqual, match: old})
		if err != nil || !holds {
			return err
		}
		swapped = e.opSet(t, key, value, req)
		return nil
	})
	return swapped, err
}

// DeleteIf deletes key only if it holds the string value, as DELEX key IFEQ
// value does, and reports whether it deleted it: a key of another type is
// DELEX's own refusal. It is logged as the DEL it performs.
func (e *Engine) DeleteIf(ctx context.Context, key, value string) (deleted bool, err error) {
	err = e.call(ctx, "DELEX", []string{key}, func() error {
		deleted, err = e.opDelex(key, "IFEQ", value, true)
		return err
	})
	return deleted, err
}

// Stats is the counters INFO renders, read from the same places, so the
// two never disagree.
type Stats struct {
	// Keys and KeysWithExpiry are INFO keyspace's db0 keys and expires.
	Keys, KeysWithExpiry int64
	// UsedMemory, PeakMemory and MaxMemory are INFO memory's used_memory,
	// used_memory_peak and maxmemory, in bytes.
	UsedMemory, PeakMemory, MaxMemory uint64
	// Commands and ErrorReplies are INFO stats' total_commands_processed and
	// total_error_replies; KeyspaceHits and KeyspaceMisses, ExpiredKeys and
	// EvictedKeys are the fields of those names.
	Commands, ErrorReplies, KeyspaceHits, KeyspaceMisses, ExpiredKeys, EvictedKeys uint64
	// AOF is INFO persistence's log: aof_enabled, aof_base_size,
	// aof_current_size, aof_rewrites, aof_rewrite_in_progress, and
	// aof_last_write_status ok.
	AOFEnabled, AOFRewriteInProgress, AOFLastWriteOK bool
	AOFBaseSize, AOFCurrentSize                      int64
	AOFRewrites                                      int
	// PeakMemoryAt is used_memory_peak_time.
	PeakMemoryAt time.Time
}

// Stats is a snapshot of e's counters, taken under its lock: no command, so
// counted as none and logged as none. A closed engine is ErrClosed.
func (e *Engine) Stats(ctx context.Context) (Stats, error) {
	if err := ctx.Err(); err != nil {
		return Stats{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return Stats{}, ErrClosed
	}
	used := e.NoteMemoryPeak()
	base, current, rewrites, _ := e.AOFStats()
	return Stats{
		Keys:                 int64(e.space.TotalKeys()),
		KeysWithExpiry:       int64(e.KeysWithExpiry()),
		UsedMemory:           used,
		PeakMemory:           e.memoryPeak,
		PeakMemoryAt:         e.memoryPeakAt,
		MaxMemory:            e.space.Limits().MaxMemory,
		Commands:             e.totals.commands,
		ErrorReplies:         e.totals.errors,
		KeyspaceHits:         e.totals.hits,
		KeyspaceMisses:       e.totals.misses,
		ExpiredKeys:          e.ExpiredKeys(),
		EvictedKeys:          e.space.Evicted(),
		AOFEnabled:           e.AOFEnabled(),
		AOFBaseSize:          base,
		AOFCurrentSize:       current,
		AOFRewrites:          rewrites,
		AOFRewriteInProgress: e.RewriteActive(),
		AOFLastWriteOK:       e.aof.failed == nil && !e.logRetrying(),
	}, nil
}
