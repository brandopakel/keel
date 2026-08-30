package core

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"memkv/internal/config"
	"memkv/internal/data_structure"
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
// Measured over a million string keys, by TestRewriteStallProfile: collecting
// the names 9ms, then 488 slices with a median of 0.5ms, then a final 10ms to
// write the keys that changed and sync the file. The work is the same as doing
// it in one go and slightly more of it, and the longest a client waits has gone
// from 186ms to about 10ms.
//
// Two stalls are left and both are the ends rather than the middle. Collecting
// the key names is one pass over the keyspace, and the final slice is one pass
// over what changed during the walk plus the fsync. Slicing those as well is
// possible and was not worth it at 8ms and 13ms; the 186ms was.
//
// Over the wire, which is the measurement that counts: 400,000 keys, a second
// connection sending PING throughout, the two versions under one harness.
//
//	                  rewrite takes    worst PING elsewhere
//	all at once             104ms                  103.8ms
//	a slice at a time       131ms                    9.4ms
//
// A client's worst wait falls elevenfold and the rewrite takes a quarter longer,
// which is the per-slice overhead. That is the trade, and it is the right way
// round: nobody is waiting on the rewrite, and everybody is waiting on the loop.

// rewriteChunk is how many keys one cycle of the walk emits.
//
// It buys stall against duration: smaller means the loop returns to its clients
// sooner and the rewrite takes more cycles to finish. 2048 is about a
// millisecond at the measured rate, which is under the latency of the disk
// write that a client's own command may be waiting on anyway.
const rewriteChunk = 2048

// rewrite is the state of the walk in progress, if there is one.
var rewrite struct {
	active  bool
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
	dirty map[string]struct{}
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

	// Anything still buffered belongs to the state about to be walked, so it
	// goes to the old log now rather than after the swap, where it would be
	// applied to a log that already contains its effect.
	if err := flushAOF(false); err != nil {
		return err
	}

	path := aof.path
	tmpPath := path + ".rewrite"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}

	rewrite.active = true
	rewrite.path = path
	rewrite.tmpPath = tmpPath
	rewrite.file = f
	rewrite.written = 0
	rewrite.pos = 0
	rewrite.dirty = make(map[string]struct{})
	rewrite.keys = allKeyNames()
	return nil
}

// AdvanceRewrite emits the next slice of the walk, and finishes if that was the
// last of it. The event loop calls it once a cycle while a rewrite is active.
func AdvanceRewrite() error {
	if !rewrite.active {
		return nil
	}

	end := rewrite.pos + rewriteChunk
	if end > len(rewrite.keys) {
		end = len(rewrite.keys)
	}

	var body []byte
	for _, key := range rewrite.keys[rewrite.pos:end] {
		// Skipped here rather than at the end: a key written during the rewrite
		// is going to be written again from its current state anyway, so
		// recording the version the walk can see is wasted bytes.
		if _, touched := rewrite.dirty[key]; touched {
			continue
		}
		body = emitKey(body, key)
	}
	rewrite.pos = end

	if err := rewriteWrite(body); err != nil {
		abortRewrite(err)
		return err
	}
	if rewrite.pos < len(rewrite.keys) {
		return nil
	}
	return finishRewrite()
}

// noteRewriteDirty records that a key was written while a rewrite is walking.
func noteRewriteDirty(key string) {
	if rewrite.active {
		rewrite.dirty[key] = struct{}{}
	}
}

// finishRewrite writes the keys that changed during the walk, then swaps the
// new log in.
func finishRewrite() error {
	var body []byte
	for key := range rewrite.dirty {
		// DEL first, unconditionally. The walk may have recorded an older
		// version of this key, and for a set or a sorted set a second record
		// would merge with the first rather than replace it - leaving members
		// that were removed during the rewrite alive again. It also covers the
		// key having changed type, and the key having gone entirely.
		body = appendCommand(body, "DEL", key)
		body = emitKey(body, key)
	}
	if err := rewriteWrite(body); err != nil {
		abortRewrite(err)
		return err
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
	syncDir(filepath.Dir(rewrite.path))

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
	return err
}

// allKeyNames collects every key in every keyspace.
func allKeyNames() []string {
	keys := make([]string, 0, data_structure.TotalKeys())
	keys = append(keys, dictStore.Keys()...)
	keys = append(keys, setStore.Keys()...)
	keys = append(keys, zsetStore.Keys()...)
	keys = append(keys, sbStore.Keys()...)
	keys = append(keys, cmsStore.Keys()...)
	keys = append(keys, morrisStore.Keys()...)
	keys = append(keys, hllStore.Keys()...)
	keys = append(keys, cfStore.Keys()...)
	return keys
}

// emitKey appends the commands that recreate one key, or nothing if no
// keyspace holds it any more.
//
// Strings, sets and sorted sets are written as the commands a client would
// send, so the log stays something a person can read. The rest have no command
// that rebuilds them and go out as bytes.
func emitKey(dst []byte, key string) []byte {
	if obj := dictStore.Peek(key); obj != nil {
		value, ok := obj.Value.(string)
		if !ok {
			return dst
		}
		dst = appendCommand(dst, "SET", key, value)
		if at, has := dictStore.ExpiryOf(key); has {
			// Written as the instant it falls due. A duration would mean
			// something different every time the log was read.
			dst = appendCommand(dst, "PEXPIREAT", key, strconv.FormatUint(at, 10))
		}
		return dst
	}
	if set, ok := setStore.Peek(key); ok {
		return appendCommand(dst, append([]string{"SADD", key}, set.Members()...)...)
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
		return appendCommand(dst, "MEMKV.RESTORE", key, string(payload))
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
	// Some filesystems refuse to sync a directory. That is not a reason to fail
	// a rewrite that has otherwise succeeded.
	_ = d.Sync()
	return nil
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
