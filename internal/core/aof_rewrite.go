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

// RewriteKeyCeiling refuses a rewrite that would not finish, rather than
// walking for the abort budget and then throwing the work away. It has to be
// set against what a rewrite actually costs, because a ceiling below the
// keyspace a server is allowed to hold is worse than no ceiling: auto-rewrite
// retries every minute, fails every time, and the log grows without bound. That
// is the same shape as the compaction ratchet in
// docs/general-performance-2026-09-06.md, reached by a different route.
//
// Measured on an M4 Pro with the log on APFS, one deliberate rewrite, no
// concurrent writers: 4.14s at a million keys, 8.20s at two million, 16.37s at
// four million - close to linear at about 4.1 seconds per million. Against the
// thirty-second budget below, four million leaves roughly half of it spare,
// which is the margin a slower disk needs.
//
// Neither the server nor an embedded engine bounds its key count by default
// (MaxKeys zero, as in Redis), so a keyspace may outgrow this; the server warns
// at startup when it logs without a -maxkeys at or below this. That is a
// documented limit of persistence rather than of the keyspace. Closing it
// needs a faster rewrite or a longer budget rather than a larger number here,
// and the number should not be raised past what has been measured.
//
// Under concurrent writes the binding constraint is rewriteDirtyKeys rather
// than duration: a rewrite that runs four times longer collects four times the
// dirty keys, so a large keyspace under load aborts on that budget first.
const RewriteKeyCeiling = 4000000

const rewriteDirtyKeys = 100000
const rewriteDirtyBytes = 8 << 20
const rewriteRecordSlice = 64 << 10

// rewriteState is the state of the walk in progress, if there is one.
type rewriteState struct {
	active          bool
	started         time.Time
	path            string
	tmpPath         string
	file            *os.File
	digest          hash.Hash
	hashCursor      *data_structure.HashCursor
	written         int64
	syncedBytes     int64
	preSyncComplete bool

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
	dirtyBytes       int
	stream           *rewriteRecord
	collectionReset  bool
	collectionActive bool
	collectionKey    string
	collectionPos    int
	collectionKind   string
}

// RewriteActive reports whether a rewrite is part-way through.
func (e *Engine) RewriteActive() bool { return e.rewrite.active }

func (e *Engine) rewriteWalkDone() bool {
	return e.rewrite.walk.Done() && e.rewrite.batchPos == len(e.rewrite.keys)
}

// StartRewrite begins one, capturing a slot limit per keyspace.
func (e *Engine) StartRewrite() error {
	if e.aof.file == nil {
		return fmt.Errorf("appendonly is off")
	}
	if e.rewrite.active {
		// Redis's words, which BGREWRITEAOF answers with.
		return fmt.Errorf("Background append only file rewriting already in progress")
	}
	if ready, _ := e.pollRewriteIO(false); !ready {
		return fmt.Errorf("previous rewrite I/O is still releasing its file")
	}
	if e.AppendPending() || (e.settings.asyncAppend && len(e.aof.buf) > 0) {
		return fmt.Errorf("rewrite waits for pending append; retry after the write reply")
	}

	// Anything still buffered belongs to the state about to be walked, so it
	// goes to the old log now rather than after the swap, where it would be
	// applied to a log that already contains its effect.
	if err := e.flushAOF(false); err != nil {
		return err
	}

	if e.keyCountForRewrite() > RewriteKeyCeiling {
		return e.refuseRewriteStart(fmt.Errorf("rewrite limit: at most %d keys", RewriteKeyCeiling))
	}
	path := e.aof.path
	tmpPath := path + ".rewrite"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return e.refuseRewriteStart(err)
	}
	aofLog("Background append only file rewriting started")

	e.rewrite.active = true
	e.rewrite.started = time.Now()
	e.rewrite.path = path
	e.rewrite.tmpPath = tmpPath
	e.rewrite.file = f
	e.rewrite.digest = nil
	if e.aof.digest != nil {
		e.rewrite.digest = sha256.New()
	}
	e.rewrite.written = 0
	e.rewrite.syncedBytes = -1
	e.rewrite.preSyncComplete = false
	e.rewrite.pos = 0
	e.rewrite.batchPos = 0
	e.rewrite.initialKeys = e.space.TotalKeys()
	e.rewrite.walk = e.space.NewKeyspaceWalk()
	e.rewrite.collectionActive = false
	e.rewrite.hashCursor = nil
	e.rewrite.dirty = make(map[string]struct{})
	e.rewrite.dirtyBytes = 0
	e.rewrite.stream = nil
	e.rewrite.keys = nil
	return nil
}

// AdvanceRewrite emits the next slice of the walk, and finishes if that was the
// last of it. The event loop calls it once a cycle while a rewrite is active.
//
// Its error is the log's, never the rewrite's: a rewrite that fails is
// abandoned, reported and retried later while the log it would have replaced
// stays the log (see aof_rewrite_status.go), so nothing a rewrite does on its
// own file stops the server.
func (e *Engine) AdvanceRewrite() error {
	if e.AppendPending() {
		return nil
	}
	e.pollAOFSync(false)
	if e.aof.failed != nil {
		return e.aof.failed
	}
	if !e.rewrite.active {
		return nil
	}

	if time.Since(e.rewrite.started) > 30*time.Second {
		e.abandonOverBudget(fmt.Errorf("rewrite exceeded its 30-second duration budget"))
		return nil // the original log continues to contain every write
	}
	if ready, err := e.pollRewriteIO(false); err != nil {
		e.abortRewrite(err)
		return nil
	} else if !ready {
		return nil
	}
	// Once the snapshot walk is done, retain changed key names until the sync
	// worker releases the old descriptor. Re-emitting hot keys every cycle
	// while replacement is blocked can make the rewrite larger than the log.
	if e.rewriteWalkDone() && !e.rewrite.collectionActive && e.aof.syncPending != nil {
		return nil
	}
	// Finish the bulk snapshot's write job before preflushing it. Dirty keys
	// accumulate during that preflush and are reconciled afterwards. Starting
	// another asynchronous dirty write on every cycle would chase hot keys
	// indefinitely and prevent the final atomic handoff.
	if e.rewriteWalkDone() && !e.rewrite.collectionActive && e.rewrite.stream == nil && !e.rewrite.preSyncComplete {
		e.startRewriteSync()
		return nil
	}
	if e.rewrite.stream != nil {
		if err := e.rewriteWrite(e.emitRewriteRecordSlice(nil)); err != nil {
			e.abortRewrite(err)
		}
		return nil
	}
	if e.rewrite.collectionActive {
		body := e.emitCollectionSlice(nil)
		if err := e.rewriteWrite(body); err != nil {
			e.abortRewrite(err)
		}
		return nil
	}
	if e.rewrite.batchPos == len(e.rewrite.keys) && !e.rewrite.walk.Done() {
		var err error
		e.rewrite.keys, _, err = e.rewrite.walk.Next(rewriteChunk, e.rewrite.keys[:0])
		e.rewrite.batchPos = 0
		if err != nil {
			e.abortRewrite(err)
			return nil
		}
	}
	deadline := time.Now().Add(time.Millisecond)
	var body []byte
	count := 0
	for e.rewrite.batchPos < len(e.rewrite.keys) && count < rewriteChunk {
		key := e.rewrite.keys[e.rewrite.batchPos]
		e.rewrite.keys[e.rewrite.batchPos] = ""
		e.rewrite.batchPos++
		e.rewrite.pos++
		count++
		if _, touched := e.rewrite.dirty[key]; !touched {
			body = e.emitRewriteKey(body, key, false)
		}
		if e.rewrite.stream != nil || e.rewrite.collectionActive || len(body) >= 1<<20 || time.Now().After(deadline) {
			break
		}
	}
	if e.rewriteWalkDone() && !e.rewrite.collectionActive && e.rewrite.stream == nil {
		for key := range e.rewrite.dirty {
			if e.rewrite.stream != nil || e.rewrite.collectionActive || count >= rewriteChunk || len(body) >= 1<<20 || time.Now().After(deadline) {
				break
			}
			body = e.emitRewriteKey(body, key, true)
			delete(e.rewrite.dirty, key)
			e.rewrite.dirtyBytes -= len(key) + 64
			count++
		}
	}
	if err := e.rewriteWrite(body); err != nil {
		e.abortRewrite(err)
		return nil
	}
	if e.rewrite.stream != nil || e.rewrite.collectionActive || !e.rewriteWalkDone() || len(e.rewrite.dirty) > 0 {
		return nil
	}

	e.finishRewrite()
	return nil
}

// Lists and sets have O(1) indexed access. Hashes retain one map cursor; sorted
// sets seek a bounded rank window in O(log(n)+chunk). Mutations invalidate the cursor; dirty-key
// reconciliation then replaces every historical fragment with DEL first.
func (e *Engine) emitRewriteKey(dst []byte, key string, reset bool) []byte {
	if obj := e.dictStore.Peek(key); obj != nil {
		commands := [][]string{{"SET", key, obj.Value}}
		if reset {
			commands = append([][]string{{"DEL", key}}, commands...)
		}
		if at, ok := e.dictStore.GetExpiry(key); ok {
			commands = append(commands, []string{"PEXPIREAT", key, strconv.FormatUint(at, 10)})
		}
		return e.appendRewriteRecords(dst, commands)
	}
	kind := ""
	if l, ok := e.listStore.Peek(key); ok && (l.Len() > 256 || l.MemUsage() > 64<<10 || len(key) > rewriteRecordSlice) {
		kind = "list"
	}
	if s, ok := e.setStore.Peek(key); ok && (s.Len() > 256 || s.MemUsage() > 64<<10 || len(key) > rewriteRecordSlice) {
		kind = "set"
	}
	if z, ok := e.zsetStore.Peek(key); ok && (z.Len() > 256 || z.MemUsage() > 64<<10 || len(key) > rewriteRecordSlice) {
		kind = "zset"
	}
	if h, ok := e.hashStore.Peek(key); ok && (h.Len() > 256 || h.MemUsage() > 64<<10 || len(key) > rewriteRecordSlice) {
		kind = "hash"
		e.rewrite.hashCursor = h.Cursor()
	}
	if kind == "" {
		// Deleted keys need only a tombstone, which may itself have a large name.
		present := false
		e.space.EachKeyspace(func(ks data_structure.Keyspace) {
			_, ok := ks.EntryBytes(key)
			present = present || ok
		})
		if !present {
			if reset {
				return e.appendRewriteRecords(dst, [][]string{{"DEL", key}})
			}
			return dst
		}
		if plan, ok := e.planOpaqueDump(key); ok && (plan.size > rewriteRecordSlice || len(key) > rewriteRecordSlice-plan.size-256) {
			var expiry uint64
			e.space.EachKeyspace(func(ks data_structure.Keyspace) {
				if at, ok := ks.GetExpiry(key); ok {
					expiry = at
				}
			})
			return e.appendOpaqueRewriteRecord(dst, key, reset, plan, expiry)
		}
		if reset {
			dst = appendCommand(dst, "DEL", key)
		}
		return e.emitKey(dst, key)
	}
	e.rewrite.collectionActive, e.rewrite.collectionKey, e.rewrite.collectionPos = true, key, 0
	e.rewrite.collectionKind = kind
	e.rewrite.collectionReset = true
	return e.emitCollectionSlice(dst)
}

func (e *Engine) emitCollectionSlice(dst []byte) []byte {
	key := e.rewrite.collectionKey
	var command string
	var length int
	var valueAt func(int) (string, string)
	var expiry func(string) (uint64, bool)
	switch e.rewrite.collectionKind {
	case "hash":
		h, ok := e.hashStore.Peek(key)
		if !ok || e.rewrite.hashCursor == nil {
			e.rewrite.collectionActive = false
			e.rewrite.hashCursor = nil
			return dst
		}
		command, length, expiry = "HSET", h.Len(), e.hashStore.GetExpiry
		valueAt = func(int) (string, string) { field, value, _ := e.rewrite.hashCursor.Entry(); return value, field }
	case "list":
		l, ok := e.listStore.Peek(key)
		if !ok {
			e.rewrite.collectionActive = false
			return dst
		}
		command, length, expiry = "RPUSH", l.Len(), e.listStore.GetExpiry
		valueAt = func(i int) (string, string) { v, _ := l.Index(i); return v, "" }
	case "set":
		s, ok := e.setStore.Peek(key)
		if !ok {
			e.rewrite.collectionActive = false
			return dst
		}
		command, length, expiry = "SADD", s.Len(), e.setStore.GetExpiry
		valueAt = func(i int) (string, string) { v, _ := s.MemberAt(i); return v, "" }
	case "zset":
		z, ok := e.zsetStore.Peek(key)
		if !ok {
			e.rewrite.collectionActive = false
			return dst
		}
		command, length, expiry = "ZADD", z.Len(), e.zsetStore.GetExpiry
		start := e.rewrite.collectionPos
		members, scores := z.RangeByRank(start, start+255, false)
		valueAt = func(i int) (string, string) { return members[i-start], formatScore(scores[i-start]) }
	}
	parts := []string{command, key}
	bytes, count := 0, 0
	deadline := time.Now().Add(time.Millisecond)
	for e.rewrite.collectionPos < length && count < 256 {
		value, score := valueAt(e.rewrite.collectionPos)
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
		e.rewrite.collectionPos++
		if command == "HSET" {
			e.rewrite.hashCursor.Advance()
		}
	}
	var commands [][]string
	if e.rewrite.collectionReset {
		commands = append(commands, []string{"DEL", key})
		e.rewrite.collectionReset = false
	}
	if count > 0 {
		commands = append(commands, parts)
	}
	if e.rewrite.collectionPos == length {
		e.rewrite.collectionActive = false
		e.rewrite.hashCursor = nil
		if at, has := expiry(key); has {
			commands = append(commands, []string{"PEXPIREAT", key, strconv.FormatUint(at, 10)})
		}
	}
	return e.appendRewriteRecords(dst, commands)
}

// noteRewriteDirty records that a key was written while a rewrite is walking.
func (e *Engine) noteRewriteDirty(key string) {
	if e.rewrite.active {
		if _, exists := e.rewrite.dirty[key]; !exists {
			charge := len(key) + 64
			if len(e.rewrite.dirty) >= rewriteDirtyKeys || charge > rewriteDirtyBytes-e.rewrite.dirtyBytes {
				e.abandonOverBudget(fmt.Errorf("rewrite exceeded dirty-key budget"))
				return
			}
			e.rewrite.dirty[key] = struct{}{}
			e.rewrite.dirtyBytes += charge
		}
		if e.rewrite.collectionActive && e.rewrite.collectionKey == key {
			// Discard the cursor, not the partial log. Reconciliation starts
			// with DEL and replaces every already-emitted fragment.
			e.rewrite.collectionActive = false
			e.rewrite.hashCursor = nil
		}
	}
}

// finishRewrite writes the keys that changed during the walk, then swaps the
// new log in.
//
// Every step up to the rename fails by abandoning the rewrite, which leaves
// the old log as it was: still the log, holding every write. After the rename
// the new file is the log whatever else happens; see aof_rewrite_status.go for
// the directory sync that follows it.
func (e *Engine) finishRewrite() {
	// Never close or replace a descriptor owned by the worker.
	if e.aof.syncPending != nil || e.pendingRewriteIO != nil {
		return
	}

	if !e.rewrite.preSyncComplete {
		e.startRewriteSync()
		return
	}
	e.rewriteFinalizeStats.active.Add(1)
	finalStarted := time.Now()
	var finalErr error
	defer func() { e.rewriteFinalizeStats.finish(finalStarted, finalErr) }()
	fail := func(err error) {
		finalErr = err
		e.abortRewrite(err)
	}
	if e.rewrite.syncedBytes != e.rewrite.written {
		// Preflush the bulk snapshot once, then synchronize only the dirty
		// suffix at the existing atomic handoff. Repeated asynchronous retries
		// could otherwise starve forever under a continuous write stream.
		if err := timedPersistenceSync(&e.rewriteFinalSyncStats, e.rewrite.file, func(f *os.File) error {
			return timedPersistenceSync(&e.rewriteSyncStats, f, e.rewriteFileSync)
		}); err != nil {
			fail(err)
			return
		}
		e.rewrite.syncedBytes = e.rewrite.written
	}
	written := e.rewrite.file
	e.rewrite.file = nil
	if err := written.Close(); err != nil {
		fail(err)
		return
	}
	// Appending must continue into the new file, not the one the old
	// descriptor points at - which, once renamed over, no longer has a name,
	// so anything written to it vanishes at the next restart. Opening it
	// before the rename means nothing after the rename can fail to reach it.
	next, err := e.rewriteOpenLog(e.rewrite.tmpPath)
	if err != nil {
		fail(err)
		return
	}

	// Rename is atomic within a directory, so a crash at any point leaves
	// either the whole old log or the whole new one, never a half-written file
	// that replay would read as a truncated tail and quietly accept.
	if err := e.rewriteRename(e.rewrite.tmpPath, e.rewrite.path); err != nil {
		if !renamedAnyway(next, e.rewrite.path) {
			next.Close()
			fail(err)
			return
		}
		aofLog("rename of %s reported %v, but the rewritten file has the log's name; appending to it", e.rewrite.tmpPath, err)
	}
	// From here the new file is the log. The directory entry has to reach disk
	// too, or a crash can leave the rename unrecorded and the old file back in
	// place; if it cannot yet, the log's next sync tries again first.
	dir := filepath.Dir(e.rewrite.path)
	if err := e.rewriteSyncDir(dir); err != nil {
		finalErr = err
		e.unsyncedLogDir = dir
		aofLog("syncing %s after the rewrite's rename: %v; the rewritten file is the log, and the next sync of it retries the directory first", dir, err)
	} else {
		e.unsyncedLogDir = "" // this sync covers an earlier rename's entry too
	}
	replaced := e.aof.file
	e.aof.file = next
	if err := replaced.Close(); err != nil {
		// Its contents are superseded by the file that now has its name.
		aofLog("closing the replaced log: %v", err)
	}
	e.aof.digest, e.aof.digestBytes = e.rewrite.digest, e.rewrite.written
	e.aof.baseSize = e.rewrite.written
	e.aof.rewriteBase = e.rewrite.written
	// The rewritten log ends with its last whole record.
	e.aof.midRecord = false
	e.appendSynced = max(e.appendSynced, e.appendCompleted)
	e.aof.written = 0
	e.aof.rewrites++
	e.aof.lastKeys = e.rewrite.initialKeys
	e.noteRewriteFinished(e.rewrite.started)

	e.rewrite.active = false
	e.rewrite.keys = nil
	e.rewrite.walk = nil
	e.rewrite.dirty = nil
	e.rewrite.dirtyBytes = 0
	e.rewrite.stream = nil
	e.rewrite.collectionKey, e.rewrite.collectionKind = "", ""
	e.rewrite.collectionActive = false
	if err := e.captureReplicationSnapshot(); err != nil {
		// The log is unaffected. A protocol 2 replica still waiting gets its
		// snapshot from a later rewrite, which its pulls start no sooner than
		// a minute from now rather than one after another.
		e.snapshotRetryAt = time.Now().Add(time.Minute)
		aofLog("replication snapshot after the rewrite: %v", err)
	}
}

// abandonOverBudget abandons a rewrite the load outran. The next automatic
// attempt waits a minute, since it would be outrun again straight away.
func (e *Engine) abandonOverBudget(cause error) {
	e.rewriteBudgetAborts++
	e.abortRewrite(cause)
	e.nextAutoRewrite = time.Now().Add(time.Minute)
}

// abortRewrite gives up on a rewrite without touching the log in use. The old
// file has had every write appended to it throughout, so abandoning the new one
// loses nothing. A cause makes it a failed rewrite, reported as Redis reports
// one; without one it is a cancellation, which Redis reports as nothing.
func (e *Engine) abortRewrite(cause error) {
	active, started, path := e.rewrite.active, e.rewrite.started, e.rewrite.path
	e.rewrite.hashCursor = nil
	e.nextAutoRewrite = time.Now().Add(rewriteRetryTick)
	workerOwnsFile := e.pendingRewriteIO != nil && e.pendingRewriteIO.abandon()
	if !workerOwnsFile && e.rewrite.file != nil {
		e.rewrite.file.Close()
	}
	if !workerOwnsFile {
		if err := os.Remove(e.rewrite.tmpPath); err != nil && !os.IsNotExist(err) {
			aofLog("rewrite temp file left behind %s: %v", e.rewrite.tmpPath, err)
		}
		e.pendingRewriteIO = nil
	}
	e.rewrite.active = false
	e.rewrite.keys = nil
	e.rewrite.walk = nil
	e.rewrite.dirty = nil
	e.rewrite.dirtyBytes = 0
	e.rewrite.stream = nil
	e.rewrite.collectionKey, e.rewrite.collectionKind = "", ""
	e.rewrite.collectionActive = false
	e.rewrite.file = nil
	if cause != nil && active {
		e.noteRewriteFailed(cause, started, path)
	}
}

// CancelRewrite abandons a rewrite in progress, for a server shutting down.
func (e *Engine) CancelRewrite() {
	if e.rewrite.active {
		e.abortRewrite(nil)
	}
}

func (e *Engine) rewriteWrite(body []byte) error {
	if len(body) == 0 {
		return nil
	}
	if e.rewrite.preSyncComplete {
		// The existing dirty-tail/handoff barrier remains synchronous. Moving
		// this phase requires ordered dual writes and a separate commit cut.
		n, err := timedPersistenceWrite(&e.rewriteWriteStats, e.rewrite.file, body, e.rewriteFileWrite)
		if e.rewrite.digest != nil {
			e.rewrite.digest.Write(body[:n])
		}
		e.rewrite.written += int64(n)
		if err == nil && n != len(body) {
			return io.ErrShortWrite
		}
		return err
	}
	e.startRewriteIO(body)
	return nil
}

// emitKey appends the commands that recreate one key, or nothing if no
// keyspace holds it any more.
//
// Strings, sets and sorted sets are written as the commands a client would
// send, so the log stays something a person can read. The rest have no command
// that rebuilds them and go out as bytes.
func (e *Engine) emitKey(dst []byte, key string) []byte {
	before := len(dst)
	dst = e.emitValue(dst, key)
	if len(dst) > before {
		e.space.EachKeyspace(func(ks data_structure.Keyspace) {
			if at, has := ks.GetExpiry(key); has {
				dst = appendCommand(dst, "PEXPIREAT", key, strconv.FormatUint(at, 10))
			}
		})
	}
	return dst
}

func (e *Engine) emitValue(dst []byte, key string) []byte {
	if obj := e.dictStore.Peek(key); obj != nil {
		return appendCommand(dst, "SET", key, obj.Value)
	}
	if set, ok := e.setStore.Peek(key); ok {
		return appendCommand(dst, append([]string{"SADD", key}, set.Members()...)...)
	}
	if h, ok := e.hashStore.Peek(key); ok {
		fields, values := h.Entries()
		parts := make([]string, 0, 2+2*len(fields))
		parts = append(parts, "HSET", key)
		for i, f := range fields {
			parts = append(parts, f, values[i])
		}
		return appendCommand(dst, parts...)
	}
	if l, ok := e.listStore.Peek(key); ok {
		// RPUSH in order, so the list rebuilds left to right exactly as it is.
		return appendCommand(dst, append([]string{"RPUSH", key}, l.All()...)...)
	}
	if zset, ok := e.zsetStore.Peek(key); ok {
		members, scores := zset.Entries()
		parts := make([]string, 0, 2+2*len(members))
		parts = append(parts, "ZADD", key)
		for i, m := range members {
			parts = append(parts, formatScore(scores[i]), m)
		}
		return appendCommand(dst, parts...)
	}
	if payload, ok := e.dumpKey(key); ok {
		return appendCommand(dst, "KEEL.RESTORE", key, string(payload))
	}
	return dst
}

// RewriteAOF runs a rewrite to completion without returning to the event loop.
//
// Used by tests, and by nothing that serves clients. A server driving the
// loop should start one and let AdvanceRewrite carry it, which is what keeps
// the stall to a slice at a time.
func (e *Engine) RewriteAOF() error {
	if err := e.StartRewrite(); err != nil {
		return err
	}
	for e.rewrite.active {
		if err := e.AdvanceRewrite(); err != nil {
			return err
		}
		// This helper is deliberately synchronous and is never the serving
		// loop. Waiting here also avoids spinning while the worker owns a write or Sync.
		if _, err := e.pollRewriteIO(true); err != nil {
			e.abortRewrite(err)
			return err
		}
	}
	// A rewrite that failed has been abandoned and reported like any other;
	// its caller is told why.
	return e.rewriteOutcome.lastErr
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
//
// After failures it waits as Redis's does; see aof_rewrite_status.go. A
// BGREWRITEAOF scheduled inside a transaction starts here too, whatever the
// automatic settings are.
func (e *Engine) maybeRewrite() {
	if e.aof.file == nil || e.rewrite.active {
		return
	}
	now := time.Now()
	if e.rewriteOutcome.scheduled {
		e.startScheduledRewrite(now)
		return
	}
	percentage := e.settings.rewritePercentage
	if now.Before(e.nextAutoRewrite) || percentage <= 0 {
		return
	}
	size := e.aof.baseSize + e.aof.written
	if size < e.settings.rewriteMinSize {
		return
	}
	if e.aof.rewriteBase > 0 {
		grown := float64(size-e.aof.rewriteBase) * 100 / float64(e.aof.rewriteBase)
		if grown < float64(percentage) {
			return
		}
	}
	if e.rewriteLimited(now) {
		return
	}
	// Redis's line, which measures growth against a base of at least one byte.
	aofLog("Starting automatic rewriting of AOF on %d%% growth", size*100/max(e.aof.rewriteBase, 1)-100)
	if err := e.StartRewrite(); err != nil {
		e.nextAutoRewrite = time.Now().Add(time.Minute)
		logStartFailure("automatic rewrite", err)
	}
}

// AOFStats reports what INFO needs to say about the log.
func (e *Engine) AOFStats() (baseSize, currentSize int64, rewrites int, keys int) {
	if e.aof.file == nil {
		return 0, 0, 0, 0
	}
	return e.aof.baseSize, e.aof.baseSize + e.aof.written, e.aof.rewrites, e.aof.lastKeys
}
