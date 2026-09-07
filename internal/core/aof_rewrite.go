package core

import (
	"crypto/sha256"
	"fmt"
	"hash"
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
// reconciliation uses the same budgets. A single key,
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
	active     bool
	started    time.Time
	path       string
	tmpPath    string
	file       *os.File
	digest     hash.Hash
	hashCursor *data_structure.HashCursor
	written    int64

	// The walker retains only one slot limit per store. keys is one bounded
	// name batch, not a snapshot of the whole keyspace.
	walk        *data_structure.KeyspaceWalk
	keys        []string
	batchPos    int
	pos         int
	initialKeys int

	// dirty is every key written since the rewrite started. These are the keys
	// the walk may have recorded wrongly, and they are all rewritten at the end
	// from whatever they hold then.
	dirty            map[string]struct{}
	collectionActive bool
	collectionKey    string
	collectionPos    int
	collectionKind   string
}

// RewriteActive reports whether a rewrite is part-way through.
func RewriteActive() bool { return rewrite.active }

func rewriteWalkDone() bool { return rewrite.walk.Done() && rewrite.batchPos == len(rewrite.keys) }

// StartRewrite begins one, capturing a slot limit per keyspace.
func StartRewrite() error {
	if aof.file == nil {
		return fmt.Errorf("appendonly is off")
	}
	if rewrite.active {
		return fmt.Errorf("a rewrite is already running")
	}
	if AppendPending() || (config.AOFAsyncAppend && len(aof.buf) > 0) {
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
	rewrite.digest = nil
	if aof.digest != nil {
		rewrite.digest = sha256.New()
	}
	rewrite.written = 0
	rewrite.pos = 0
	rewrite.batchPos = 0
	rewrite.initialKeys = data_structure.TotalKeys()
	rewrite.walk = data_structure.NewKeyspaceWalk()
	rewrite.collectionActive = false
	rewrite.hashCursor = nil
	rewrite.dirty = make(map[string]struct{})
	rewrite.keys = nil
	return nil
}

// AdvanceRewrite emits the next slice of the walk, and finishes if that was the
// last of it. The event loop calls it once a cycle while a rewrite is active.
func AdvanceRewrite() error {
	if AppendPending() {
		return nil
	}
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
	if rewriteWalkDone() && !rewrite.collectionActive && aof.syncPending != nil {
		return nil
	}
	if rewrite.collectionActive {
		body := emitCollectionSlice(nil)
		if err := rewriteWrite(body); err != nil {
			abortRewrite(err)
			return err
		}
		return nil
	}
	if rewrite.batchPos == len(rewrite.keys) && !rewrite.walk.Done() {
		var err error
		rewrite.keys, _, err = rewrite.walk.Next(rewriteChunk, rewrite.keys[:0])
		rewrite.batchPos = 0
		if err != nil {
			abortRewrite(err)
			return err
		}
	}
	deadline := time.Now().Add(time.Millisecond)
	var body []byte
	count := 0
	for rewrite.batchPos < len(rewrite.keys) && count < rewriteChunk {
		key := rewrite.keys[rewrite.batchPos]
		rewrite.batchPos++
		rewrite.pos++
		count++
		if _, touched := rewrite.dirty[key]; !touched {
			body = emitRewriteKey(body, key)
		}
		if rewrite.collectionActive || len(body) >= 1<<20 || time.Now().After(deadline) {
			break
		}
	}
	if rewriteWalkDone() && !rewrite.collectionActive {
		for key := range rewrite.dirty {
			if rewrite.collectionActive || count >= rewriteChunk || len(body) >= 1<<20 || time.Now().After(deadline) {
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
	if rewrite.collectionActive || !rewriteWalkDone() || len(rewrite.dirty) > 0 {
		return nil
	}

	return finishRewrite()
}

// Lists and sets have O(1) indexed access. Hashes retain one map cursor; sorted
// sets seek a bounded rank window in O(log(n)+chunk). Mutations invalidate the cursor; dirty-key
// reconciliation then replaces every historical fragment with DEL first.
func emitRewriteKey(dst []byte, key string) []byte {
	kind := ""
	if l, ok := listStore.Peek(key); ok && (l.Len() > 256 || l.MemUsage() > 64<<10) {
		kind = "list"
	}
	if s, ok := setStore.Peek(key); ok && (s.Len() > 256 || s.MemUsage() > 64<<10) {
		kind = "set"
	}
	if z, ok := zsetStore.Peek(key); ok && (z.Len() > 256 || z.MemUsage() > 64<<10) {
		kind = "zset"
	}
	if h, ok := hashStore.Peek(key); ok && (h.Len() > 256 || h.MemUsage() > 64<<10) {
		kind = "hash"
		rewrite.hashCursor = h.Cursor()
	}
	if kind == "" {
		return emitKey(dst, key)
	}
	rewrite.collectionActive, rewrite.collectionKey, rewrite.collectionPos = true, key, 0
	rewrite.collectionKind = kind
	return emitCollectionSlice(appendCommand(dst, "DEL", key))
}

func emitCollectionSlice(dst []byte) []byte {
	key := rewrite.collectionKey
	var command string
	var length int
	var valueAt func(int) (string, string)
	var expiry func(string) (uint64, bool)
	switch rewrite.collectionKind {
	case "hash":
		h, ok := hashStore.Peek(key)
		if !ok || rewrite.hashCursor == nil {
			rewrite.collectionActive = false
			rewrite.hashCursor = nil
			return dst
		}
		command, length, expiry = "HSET", h.Len(), hashStore.GetExpiry
		valueAt = func(int) (string, string) { field, value, _ := rewrite.hashCursor.Entry(); return value, field }
	case "list":
		l, ok := listStore.Peek(key)
		if !ok {
			rewrite.collectionActive = false
			return dst
		}
		command, length, expiry = "RPUSH", l.Len(), listStore.GetExpiry
		valueAt = func(i int) (string, string) { v, _ := l.Index(i); return v, "" }
	case "set":
		s, ok := setStore.Peek(key)
		if !ok {
			rewrite.collectionActive = false
			return dst
		}
		command, length, expiry = "SADD", s.Len(), setStore.GetExpiry
		valueAt = func(i int) (string, string) { v, _ := s.MemberAt(i); return v, "" }
	case "zset":
		z, ok := zsetStore.Peek(key)
		if !ok {
			rewrite.collectionActive = false
			return dst
		}
		command, length, expiry = "ZADD", z.Len(), zsetStore.GetExpiry
		start := rewrite.collectionPos
		members, scores := z.RangeByRank(start, start+255, false)
		valueAt = func(i int) (string, string) { return members[i-start], formatScore(scores[i-start]) }
	}
	parts := []string{command, key}
	bytes, count := 0, 0
	deadline := time.Now().Add(time.Millisecond)
	for rewrite.collectionPos < length && count < 256 {
		value, score := valueAt(rewrite.collectionPos)
		size := len(value) + len(score) + 32
		// An individual member may exceed the target; emit it alone so a
		// cursor always advances. Input/per-key limits still bound that case.
		if count > 0 && (bytes+size > 64<<10 || time.Now().After(deadline)) {
			break
		}
		if command == "ZADD" || command == "HSET" {
			parts = append(parts, score)
		}
		parts = append(parts, value)
		bytes += size
		count++
		rewrite.collectionPos++
		if command == "HSET" {
			rewrite.hashCursor.Advance()
		}
	}
	if count > 0 {
		dst = appendCommand(dst, parts...)
	}
	if rewrite.collectionPos == length {
		rewrite.collectionActive = false
		rewrite.hashCursor = nil
		if at, has := expiry(key); has {
			dst = appendCommand(dst, "PEXPIREAT", key, strconv.FormatUint(at, 10))
		}
	}
	return dst
}

// noteRewriteDirty records that a key was written while a rewrite is walking.
func noteRewriteDirty(key string) {
	if rewrite.active {
		rewrite.dirty[key] = struct{}{}
		if rewrite.collectionActive && rewrite.collectionKey == key {
			// Discard the cursor, not the partial log. Reconciliation starts
			// with DEL and replaces every already-emitted fragment.
			rewrite.collectionActive = false
			rewrite.hashCursor = nil
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
	aof.digest, aof.digestBytes = rewrite.digest, rewrite.written
	aof.baseSize = rewrite.written
	aof.rewriteBase = rewrite.written
	appendSynced = max(appendSynced, appendCompleted)
	aof.written = 0
	aof.rewrites++
	aof.lastKeys = rewrite.initialKeys

	rewrite.active = false
	rewrite.keys = nil
	rewrite.walk = nil
	rewrite.dirty = nil
	return captureReplicationSnapshot()
}

// abortRewrite gives up on a rewrite without touching the log in use. The old
// file has had every write appended to it throughout, so abandoning the new one
// loses nothing.
func abortRewrite(cause error) {
	rewrite.hashCursor = nil
	nextAutoRewrite = time.Now().Add(time.Minute)
	if rewrite.file != nil {
		rewrite.file.Close()
	}
	os.Remove(rewrite.tmpPath)
	rewrite.active = false
	rewrite.keys = nil
	rewrite.walk = nil
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
	if rewrite.digest != nil {
		rewrite.digest.Write(body[:n])
	}
	rewrite.written += int64(n)
	if err == nil && n != len(body) {
		return io.ErrShortWrite
	}
	return err
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
		return appendCommand(dst, "SET", key, obj.Value)
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
	if aof.rewriteBase > 0 {
		grown := float64(size-aof.rewriteBase) * 100 / float64(aof.rewriteBase)
		if grown < float64(config.AOFAutoRewritePercentage) {
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
