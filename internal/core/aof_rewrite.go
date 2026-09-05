package core

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/brandopakel/keel/internal/config"
	"github.com/brandopakel/keel/internal/data_structure"
)

// Rewriting the append-only file, a slice at a time.
//
// The log records commands, so it grows with traffic rather than with data. A
// key written a million times is a million lines that one line reproduces, and
// startup replays every one of them - so an untouched server gets slower to
// start for as long as it runs. A rewrite writes the shortest log producing the
// current state, then swaps it in.
//
// # Doing it without stopping
//
// Redis forks. The child walks a copy-on-write snapshot while the parent keeps
// serving, and the writes that arrive meanwhile are buffered and appended when
// the child finishes. A Go program cannot fork that way, and the first version
// of this did the whole walk in one go instead: 186ms of silence per million
// keys, on the thread that serves every client.
//
// The walk is now spread across event-loop cycles, a few thousand keys at a
// time. That alone would be wrong, because the keyspace moves underneath a walk
// that takes many cycles: a key written after the walk passed it is recorded at
// its old value, and one created afterwards is not recorded at all.
//
// The fix does not need a consistent snapshot, which is the part worth stating
// plainly. Every key written during a rewrite is remembered, and once the walk
// finishes each of those keys is written again from its current state, preceded
// by a DEL so the later record replaces the earlier rather than merging with
// it. Whatever the walk saw for those keys - stale, half-built, or nothing at
// all - is overwritten by what is true at the end. Keys nobody touched cannot
// be stale, because nothing touched them.
//
// So the walk may see a moving keyspace and still produce an exact log, and
// what it costs is one pass over the keys written during the rewrite rather
// than a copy of the keyspace.
//
// Slices stop between keys after 2048 keys, 1 MiB or 1 ms. Dirty-key
// reconciliation uses the same budgets. Snapshot enumeration, a single key,
// filesystem writes and the final sync can exceed the time target. Admission
// and duration limits prevent an unbounded snapshot or endlessly growing walk.

// rewriteChunk is how many keys one cycle of the walk emits.
//
// It buys stall against duration: smaller means the loop returns to its clients
// sooner and the rewrite takes more cycles to finish. 2048 is about a
// millisecond at the measured rate, which is under the latency of the disk
// write that a client's own command may be waiting on anyway.
const rewriteChunk = 2048

var nextAutoRewrite time.Time

// rewrite is the state of the walk in progress, if there is one.
var rewrite struct {
	active  bool
	started time.Time
	path    string
	tmpPath string
	file    *os.File
	written int64

	// keys is every name that existed when the rewrite started. A Go map
	// cannot be iterated across cycles - there is no resumable iterator - so
	// the names are collected up front and walked as a list.
	keys []string
	pos  int

	// dirty is every key written since the rewrite started. These are the keys
	// the walk may have recorded wrongly, and they are all rewritten at the end
	// from whatever they hold then.
	dirty      map[string]struct{}
	listActive bool
	listKey    string
	listPos    int
}

// RewriteActive reports whether a rewrite is part-way through.
func RewriteActive() bool { return rewrite.active }

// StartRewrite begins one, collecting the key names it will walk.
func StartRewrite() error {
	if aof.file == nil {
		return fmt.Errorf("appendonly is off")
	}
	if rewrite.active {
		return fmt.Errorf("a rewrite is already running")
	}
	if config.AOFAsyncAppend && (AppendPending() || len(aof.buf) > 0) {
		return fmt.Errorf("rewrite waits for pending append; retry after the write reply")
	}

	// Anything still buffered belongs to the state about to be walked, so it
	// goes to the old log now rather than after the swap, where it would be
	// applied to a log that already contains its effect.
	if err := flushAOF(false); err != nil {
		return err
	}

	if data_structure.TotalKeys() > 1000000 {
		return fmt.Errorf("rewrite snapshot limit: at most 1000000 keys")
	}
	path := aof.path
	tmpPath := path + ".rewrite"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}

	rewrite.active = true
	rewrite.started = time.Now()
	rewrite.path = path
	rewrite.tmpPath = tmpPath
	rewrite.file = f
	rewrite.written = 0
	rewrite.pos = 0
	rewrite.listActive = false
	rewrite.dirty = make(map[string]struct{})
	rewrite.keys = allKeyNames()
	return nil
}

// AdvanceRewrite emits the next slice of the walk, and finishes if that was the
// last of it. The event loop calls it once a cycle while a rewrite is active.
func AdvanceRewrite() error {
	pollAOFSync(false)
	if aof.failed != nil {
		return aof.failed
	}
	if !rewrite.active {
		return nil
	}

	if time.Since(rewrite.started) > 30*time.Second || len(rewrite.dirty) > 100000 {
		abortRewrite(fmt.Errorf("rewrite exceeded duration or dirty-key budget"))
		return nil // the original log continues to contain every write
	}
	// Once the snapshot walk is done, retain changed key names until the sync
	// worker releases the old descriptor. Re-emitting hot keys every cycle
	// while replacement is blocked can make the rewrite larger than the log.
	if rewrite.pos == len(rewrite.keys) && !rewrite.listActive && aof.syncPending != nil {
		return nil
	}
	if rewrite.listActive {
		body := emitListSlice(nil)
		if err := rewriteWrite(body); err != nil {
			abortRewrite(err)
			return err
		}
		return nil
	}
	deadline := time.Now().Add(time.Millisecond)
	var body []byte
	count := 0
	for rewrite.pos < len(rewrite.keys) && count < rewriteChunk {
		key := rewrite.keys[rewrite.pos]
		rewrite.pos++
		count++
		if _, touched := rewrite.dirty[key]; !touched {
			body = emitRewriteKey(body, key)
		}
		if rewrite.listActive || len(body) >= 1<<20 || time.Now().After(deadline) {
			break
		}
	}
	if rewrite.pos == len(rewrite.keys) && !rewrite.listActive {
		for key := range rewrite.dirty {
			if rewrite.listActive || count >= rewriteChunk || len(body) >= 1<<20 || time.Now().After(deadline) {
				break
			}
			body = appendCommand(body, "DEL", key)
			body = emitRewriteKey(body, key)
			delete(rewrite.dirty, key)
			count++
		}
	}
	if err := rewriteWrite(body); err != nil {
		abortRewrite(err)
		return err
	}
	if rewrite.listActive || rewrite.pos < len(rewrite.keys) || len(rewrite.dirty) > 0 {
		return nil
	}

	return finishRewrite()
}

// Large lists have O(1) indexed access, so they can be serialized across
// turns without copying a snapshot. A mutation invalidates the cursor.
func emitRewriteKey(dst []byte, key string) []byte {
	if l, ok := listStore.Peek(key); ok && l.Len() > 256 {
		rewrite.listActive, rewrite.listKey, rewrite.listPos = true, key, 0
		return emitListSlice(appendCommand(dst, "DEL", key))
	}
	return emitKey(dst, key)
}

func emitListSlice(dst []byte) []byte {
	key := rewrite.listKey
	l, ok := listStore.Peek(key)
	if !ok {
		rewrite.listActive = false
		return dst
	}
	parts := []string{"RPUSH", key}
	bytes := 0
	for rewrite.listPos < l.Len() && len(parts) < 258 && bytes < 64<<10 {
		value, _ := l.Index(rewrite.listPos)
		parts = append(parts, value)
		bytes += len(value)
		rewrite.listPos++
	}
	dst = appendCommand(dst, parts...)
	if rewrite.listPos == l.Len() {
		rewrite.listActive = false
		if at, has := listStore.GetExpiry(key); has {
			dst = appendCommand(dst, "PEXPIREAT", key, strconv.FormatUint(at, 10))
		}
	}
	return dst
}

// noteRewriteDirty records that a key was written while a rewrite is walking.
func noteRewriteDirty(key string) {
	if rewrite.active {
		rewrite.dirty[key] = struct{}{}
		if rewrite.listActive && rewrite.listKey == key {
			// Discard the cursor, not the partial log. Reconciliation starts
			// with DEL and replaces every already-emitted fragment.
			rewrite.listActive = false
		}
	}
}

// finishRewrite writes the keys that changed during the walk, then swaps the
// new log in.
func finishRewrite() error {
	// Never close or replace a descriptor owned by the worker.
	if aof.syncPending != nil {
		return nil
	}

	if err := rewrite.file.Sync(); err != nil {
		abortRewrite(err)
		return err
	}
	if err := rewrite.file.Close(); err != nil {
		abortRewrite(err)
		return err
	}
	rewrite.file = nil

	// Rename is atomic within a directory, so a crash at any point leaves
	// either the whole old log or the whole new one, never a half-written file
	// that replay would read as a truncated tail and quietly accept.
	if err := os.Rename(rewrite.tmpPath, rewrite.path); err != nil {
		abortRewrite(err)
		return err
	}
	// The directory entry has to reach disk too, or a crash can leave the
	// rename unrecorded and the old file back in place.
	if err := syncDir(filepath.Dir(rewrite.path)); err != nil {
		aof.failed = err
		return err
	}

	// Appending must continue into the file that is now there, not the one the
	// old descriptor still points at - which, having been renamed over, no
	// longer has a name at all, so its contents vanish at the next restart.
	if err := aof.file.Close(); err != nil {
		return err
	}
	f, err := os.OpenFile(rewrite.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		aof.file = nil
		rewrite.active = false
		return err
	}
	aof.file = f
	aof.baseSize = rewrite.written
	aof.written = 0
	aof.rewrites++
	aof.lastKeys = len(rewrite.keys)

	rewrite.active = false
	rewrite.keys = nil
	rewrite.dirty = nil
	return nil
}

// abortRewrite gives up on a rewrite without touching the log in use. The old
// file has had every write appended to it throughout, so abandoning the new one
// loses nothing.
func abortRewrite(cause error) {
	nextAutoRewrite = time.Now().Add(time.Minute)
	if rewrite.file != nil {
		rewrite.file.Close()
	}
	os.Remove(rewrite.tmpPath)
	rewrite.active = false
	rewrite.keys = nil
	rewrite.dirty = nil
	rewrite.file = nil
	if cause != nil {
		aofLog("rewrite abandoned: %v", cause)
	}
}

// CancelRewrite abandons a rewrite in progress, for a server shutting down.
func CancelRewrite() {
	if rewrite.active {
		abortRewrite(nil)
	}
}

func rewriteWrite(body []byte) error {
	if len(body) == 0 {
		return nil
	}
	n, err := rewrite.file.Write(body)
	rewrite.written += int64(n)
	if err == nil && n != len(body) {
		return io.ErrShortWrite
	}
	return err
}

// allKeyNames collects every key in every keyspace.
// allKeyNames collects every key the rewrite has to consider.
//
// Asked of the keyspace registry rather than of each store by name. The list of
// stores written out by hand was one entry short the first time a type was
// added after it - hashes - and the walk silently wrote nothing for them, so a
// rewrite dropped every hash in the keyspace. That is the same shape as DBSIZE
// counting only strings: a list of the stores that has to be kept in step with
// the stores, in a file that has no reason to notice when it is not.
func allKeyNames() []string {
	keys := make([]string, 0, data_structure.TotalKeys())
	data_structure.EachKeyspace(func(ks data_structure.Keyspace) {
		keys = append(keys, ks.Keys()...)
	})
	return keys
}

// emitKey appends the commands that recreate one key, or nothing if no
// keyspace holds it any more.
//
// Strings, sets and sorted sets are written as the commands a client would
// send, so the log stays something a person can read. The rest have no command
// that rebuilds them and go out as bytes.
func emitKey(dst []byte, key string) []byte {
	before := len(dst)
	dst = emitValue(dst, key)
	if len(dst) > before {
		data_structure.EachKeyspace(func(ks data_structure.Keyspace) {
			if at, has := ks.GetExpiry(key); has {
				dst = appendCommand(dst, "PEXPIREAT", key, strconv.FormatUint(at, 10))
			}
		})
	}
	return dst
}

func emitValue(dst []byte, key string) []byte {
	if obj := dictStore.Peek(key); obj != nil {
		value, ok := obj.Value.(string)
		if !ok {
			return dst
		}
		dst = appendCommand(dst, "SET", key, value)

		return dst
	}
	if set, ok := setStore.Peek(key); ok {
		return appendCommand(dst, append([]string{"SADD", key}, set.Members()...)...)
	}
	if h, ok := hashStore.Peek(key); ok {
		fields, values := h.Entries()
		parts := make([]string, 0, 2+2*len(fields))
		parts = append(parts, "HSET", key)
		for i, f := range fields {
			parts = append(parts, f, values[i])
		}
		return appendCommand(dst, parts...)
	}
	if l, ok := listStore.Peek(key); ok {
		// RPUSH in order, so the list rebuilds left to right exactly as it is.
		return appendCommand(dst, append([]string{"RPUSH", key}, l.All()...)...)
	}
	if zset, ok := zsetStore.Peek(key); ok {
		members, scores := zset.Entries()
		parts := make([]string, 0, 2+2*len(members))
		parts = append(parts, "ZADD", key)
		for i, m := range members {
			parts = append(parts, formatScore(scores[i]), m)
		}
		return appendCommand(dst, parts...)
	}
	if payload, ok := dumpKey(key); ok {
		return appendCommand(dst, "KEEL.RESTORE", key, string(payload))
	}
	return dst
}

// RewriteAOF runs a rewrite to completion without returning to the event loop.
//
// Used by tests, and by nothing that serves clients. A server driving the
// loop should start one and let AdvanceRewrite carry it, which is what keeps
// the stall to a slice at a time.
func RewriteAOF() error {
	if err := StartRewrite(); err != nil {
		return err
	}
	for rewrite.active {
		if err := AdvanceRewrite(); err != nil {
			return err
		}
	}
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// maybeRewrite starts a rewrite if the log has grown past the configured share
// of what it was after the last one.
//
// The percentage is measured against the size the log started at rather than
// against a fixed number, because what matters is how much of the file is
// superseded rather than how large it is: a 100MB log for 100MB of data has
// nothing to gain from being rewritten, and a 100MB log for 1MB of data is
// almost entirely history.
func maybeRewrite() {
	if time.Now().Before(nextAutoRewrite) {
		return
	}
	if aof.file == nil || rewrite.active || config.AOFAutoRewritePercentage <= 0 {
		return
	}
	size := aof.baseSize + aof.written
	if size < config.AOFAutoRewriteMinSize {
		return
	}
	if aof.baseSize > 0 {
		grown := (size - aof.baseSize) * 100 / aof.baseSize
		if grown < int64(config.AOFAutoRewritePercentage) {
			return
		}
	}
	if err := StartRewrite(); err != nil {
		nextAutoRewrite = time.Now().Add(time.Minute)
		aofLog("automatic rewrite failed to start: %v", err)
	}
}

// AOFStats reports what INFO needs to say about the log.
func AOFStats() (baseSize, currentSize int64, rewrites int, keys int) {
	if aof.file == nil {
		return 0, 0, 0, 0
	}
	return aof.baseSize, aof.baseSize + aof.written, aof.rewrites, aof.lastKeys
}
