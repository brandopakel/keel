package keel

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// The Keel-native methods: what an embedded cache wants and Redis's command
// set has no single command for, built on the commands that do the work.

// flight is one GetOrSet load, which every caller asking for its key while it
// runs waits on.
type flight struct {
	done  chan struct{}
	value string
	err   error
}

// errLoadPanicked is a GetOrSet load that panicked: every caller waiting on
// it is answered with it, the panic's value after it.
var errLoadPanicked = errors.New("keel: GetOrSet's load panicked")

// GetOrSet is the string key holds, or, when it holds nothing, the string
// load returns, which it stores, as SET with opts stores one: the cache-aside
// read, done once for every caller at a time. Keel-native: no Redis command
// does this; it is GET, then SET NX, and is logged as the SET (and its
// PEXPIREAT) it performs. A WithTTL runs from when the value is stored, not
// from when the load began.
//
// Concurrent callers that miss the same key share one load: the first starts
// it and the rest wait for it, and all answer with what it returned. A load
// that fails stores nothing, and every waiter returns its error; the next
// call loads again. A caller whose context is done stops waiting and returns
// the context's error, without cancelling the load for the others: the load
// runs with a context that carries the first caller's values and is never
// cancelled. If the key was set by another writer while the load ran, the
// stored value wins, as SET NX leaves it, and is what every waiter answers.
// A key of another type is ErrWrongType, and nothing loads.
func (db *DB) GetOrSet(ctx context.Context, key string, load func(context.Context) (string, error), opts ...SetOption) (string, error) {
	value, err := db.Get(ctx, key)
	if !errors.Is(err, ErrNotFound) {
		return value, err
	}
	// Refused before anything loads; set again once the load is done, so
	// that a TTL runs from when the value is stored, as SET's PX does.
	if _, err := setOptions(opts, true, false); err != nil {
		return "", err
	}
	db.mu.Lock()
	f, running := db.flights[key]
	if !running {
		f = &flight{done: make(chan struct{})}
		if db.flights == nil {
			db.flights = map[string]*flight{}
		}
		db.flights[key] = f
		go db.runLoad(context.WithoutCancel(ctx), key, f, load, opts)
	}
	db.mu.Unlock()
	select {
	case <-f.done:
		return f.value, f.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// runLoad runs one GetOrSet load and stores what it returns, then answers
// every waiter.
func (db *DB) runLoad(ctx context.Context, key string, f *flight, load func(context.Context) (string, error), opts []SetOption) {
	defer func() {
		if p := recover(); p != nil {
			f.err = fmt.Errorf("%w: %v", errLoadPanicked, p)
		}
		db.mu.Lock()
		delete(db.flights, key)
		db.mu.Unlock()
		close(f.done)
	}()
	value, err := load(ctx)
	if err != nil {
		f.err = err
		return
	}
	stored, err := db.set(ctx, key, value, opts, true, false)
	if err != nil {
		f.err = err
		return
	}
	if !stored {
		// Another writer set the key while the load ran; what it stored is
		// the cache's value now.
		if current, err := db.Get(ctx, key); err == nil {
			value = current
		} else if !errors.Is(err, ErrNotFound) {
			f.err = err
			return
		}
	}
	f.value = value
}

// CompareAndSwap stores value at key only if key holds the string old, and
// reports whether it stored it; a key that holds nothing or another string is
// left. Keel-native: it is Redis 8's SET key value IFEQ old, with opts as
// SET's - a swap clears the key's expiry unless an option sets or keeps one -
// and is logged as the SET it performs. A key of another type is
// ErrWrongType.
func (db *DB) CompareAndSwap(ctx context.Context, key, old, value string, opts ...SetOption) (bool, error) {
	o, err := setOptions(opts, false, false)
	if err != nil {
		return false, err
	}
	swapped, err := db.e.CompareAndSwap(ctx, key, old, value, o)
	return swapped, wrap(err)
}

// DeleteIf deletes key only if it holds the string value, and reports whether
// it deleted it. Keel-native: it is Redis 8's DELEX key IFEQ value, logged as
// the DEL it performs. A key of another type is DELEX's refusal, in its words,
// which errors.Is matches with ErrWrongType.
func (db *DB) DeleteIf(ctx context.Context, key, value string) (bool, error) {
	deleted, err := db.e.DeleteIf(ctx, key, value)
	return deleted, wrap(err)
}

// Stats is a snapshot of an instance's counters. Keel-native: the fields INFO
// renders as text, read from the same counters, so the two never disagree.
type Stats struct {
	// Keys counts the keys, and KeysWithExpiry those that expire.
	Keys, KeysWithExpiry int64
	// UsedMemory is the bytes the instance holds, PeakMemory the most it has
	// held, at PeakMemoryAt, and MaxMemory its bound, zero for none.
	UsedMemory, PeakMemory, MaxMemory uint64
	PeakMemoryAt                      time.Time
	// Commands counts the calls run, ErrorReplies those that failed.
	Commands, ErrorReplies uint64
	// KeyspaceHits and KeyspaceMisses count the keys reads looked up and
	// found, or did not.
	KeyspaceHits, KeyspaceMisses uint64
	// ExpiredKeys and EvictedKeys count the keys expiry and the memory bound
	// removed.
	ExpiredKeys, EvictedKeys uint64
	// Persistence is the log's state; nil for an instance in memory alone.
	Persistence *PersistenceStats
}

// PersistenceStats is the state of an instance's log.
type PersistenceStats struct {
	// Size is the log's size in bytes, and BaseSize its size after the last
	// rewrite, from which the next automatic one is reckoned.
	Size, BaseSize int64
	// Rewrites counts the rewrites, and RewriteInProgress says one is running.
	Rewrites          int
	RewriteInProgress bool
	// LastWriteOK is false while a failed write or sync is retried, or after
	// one has failed for good.
	LastWriteOK bool
}

// Stats is a snapshot of the instance's counters, which counts as no
// command: see Stats.
func (db *DB) Stats(ctx context.Context) (Stats, error) {
	s, err := db.e.Stats(ctx)
	if err != nil {
		return Stats{}, wrap(err)
	}
	out := Stats{
		Keys: s.Keys, KeysWithExpiry: s.KeysWithExpiry,
		UsedMemory: s.UsedMemory, PeakMemory: s.PeakMemory, MaxMemory: s.MaxMemory, PeakMemoryAt: s.PeakMemoryAt,
		Commands: s.Commands, ErrorReplies: s.ErrorReplies,
		KeyspaceHits: s.KeyspaceHits, KeyspaceMisses: s.KeyspaceMisses,
		ExpiredKeys: s.ExpiredKeys, EvictedKeys: s.EvictedKeys,
	}
	if s.AOFEnabled {
		out.Persistence = &PersistenceStats{Size: s.AOFCurrentSize, BaseSize: s.AOFBaseSize, Rewrites: s.AOFRewrites,
			RewriteInProgress: s.AOFRewriteInProgress, LastWriteOK: s.AOFLastWriteOK}
	}
	return out, nil
}
