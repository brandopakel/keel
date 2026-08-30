package core

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"memkv/internal/config"
	"memkv/internal/data_structure"
)

// Rewriting the append-only file.
//
// The log records commands, so it grows with traffic rather than with data. A
// key written a million times is a million lines that one line would reproduce,
// and startup replays every one of them - so an untouched server gets slower to
// start for as long as it runs, and eventually the log is larger than the
// machine. Redis calls the fix BGREWRITEAOF: write the shortest log producing
// the current state, then swap it in.
//
// Shortest here means one command per key. A string is a SET, a set is one SADD
// of its members, a sorted set one ZADD. The five types whose state no command
// can rebuild - the two filters, the two sketches and the HyperLogLog - are
// written with MEMKV.RESTORE, which is what that command exists for.
//
// # This one blocks, and Redis's does not
//
// Redis forks. The child writes the snapshot from a copy-on-write view while
// the parent keeps serving, and the writes that arrive meanwhile are buffered
// and appended when the child finishes. A Go program cannot fork that way - the
// runtime's threads do not survive it - so the choice here is between blocking
// the loop and building the buffering and hand-off by hand.
//
// This blocks. The cost is one pass over the keyspace plus one write and one
// fsync, on the thread that also serves every client, so it is a stall
// proportional to the data rather than to the log. That is the same shape of
// problem as LCS and is bounded the same way: by knowing the number and saying
// it. Measured by BenchmarkRewrite on darwin/arm64: 10ms for ten thousand
// string keys, 20ms for a hundred thousand, 186ms for a million - a floor of
// about 10ms that is the fsync, and 5.4 million keys a second above it.
//
// A tenth of a second of silence every time the log doubles is a real cost and
// the reason the automatic trigger defaults to only firing past 64MB. Doing it
// without the stall means buffering the writes that arrive during the rewrite
// and applying them at the end, which is the next piece of work rather than
// this one.

// rewriteInProgress guards against re-entering the rewrite from inside itself,
// which would otherwise be possible through the eviction that writing can
// trigger.
var rewriteInProgress bool

// RewriteAOF writes the shortest log that reproduces the current keyspace and
// puts it in place of the current one.
//
// The new log is built beside the old and renamed over it. Rename is atomic
// within a directory, so a crash at any point leaves either the whole old log
// or the whole new one - never a half-written file that replay would read as a
// truncated tail and silently accept.
func RewriteAOF() error {
	if aof.file == nil {
		return fmt.Errorf("appendonly is off")
	}
	if rewriteInProgress {
		return fmt.Errorf("a rewrite is already running")
	}
	rewriteInProgress = true
	defer func() { rewriteInProgress = false }()

	// Anything still buffered belongs to the state about to be written out, and
	// writing it afterwards would apply it twice.
	if err := FlushAOF(); err != nil {
		return err
	}

	body, keys, err := marshalKeyspace()
	if err != nil {
		return err
	}

	path := aof.path
	tmp := path + ".rewrite"
	if err := writeAndSync(tmp, body); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	// The directory entry itself has to reach disk, or a crash can leave the
	// rename unrecorded and the old file back in place. This is the step that
	// is easy to leave out and impossible to notice until it matters.
	if err := syncDir(filepath.Dir(path)); err != nil {
		return err
	}

	// Appending must continue into the file that is now there, not the one the
	// old descriptor still points at - which, having been renamed over, no
	// longer has a name at all.
	if err := aof.file.Close(); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		aof.file = nil
		return err
	}
	aof.file = f
	aof.baseSize = int64(len(body))
	aof.written = 0
	aof.rewrites++
	aof.lastKeys = keys
	return nil
}

// marshalKeyspace renders every key as the command that recreates it.
func marshalKeyspace() ([]byte, int, error) {
	var out []byte
	keys := 0

	// Strings carry an expiry, and it is written as an instant for the same
	// reason EXPIRE is: a duration in a file means something different every
	// time the file is read.
	for _, key := range dictStore.Keys() {
		obj := dictStore.Peek(key)
		if obj == nil {
			continue
		}
		value, ok := obj.Value.(string)
		if !ok {
			continue
		}
		out = appendCommand(out, "SET", key, value)
		if at, has := dictStore.ExpiryOf(key); has {
			out = appendCommand(out, "PEXPIREAT", key, strconv.FormatUint(at, 10))
		}
		keys++
	}

	// A set and a sorted set are one command each, however many members they
	// hold. A very large one therefore becomes a very large command - bounded
	// by the same maxBulkLength the parser enforces on the way back in.
	for _, key := range setStore.Keys() {
		set, ok := setStore.Peek(key)
		if !ok {
			continue
		}
		out = appendCommand(out, append([]string{"SADD", key}, set.Members()...)...)
		keys++
	}
	for _, key := range zsetStore.Keys() {
		zset, ok := zsetStore.Peek(key)
		if !ok {
			continue
		}
		members, scores := zset.Entries()
		parts := make([]string, 0, 2+2*len(members))
		parts = append(parts, "ZADD", key)
		for i, m := range members {
			parts = append(parts, formatScore(scores[i]), m)
		}
		out = appendCommand(out, parts...)
		keys++
	}

	// Everything else goes out as bytes, because nothing else can reproduce it.
	for _, key := range opaqueKeys() {
		payload, ok := dumpKey(key)
		if !ok {
			continue
		}
		out = appendCommand(out, "MEMKV.RESTORE", key, string(payload))
		keys++
	}
	return out, keys, nil
}

// opaqueKeys lists the keys whose state only MEMKV.RESTORE can carry.
func opaqueKeys() []string {
	var keys []string
	keys = append(keys, sbStore.Keys()...)
	keys = append(keys, cmsStore.Keys()...)
	keys = append(keys, morrisStore.Keys()...)
	keys = append(keys, hllStore.Keys()...)
	keys = append(keys, cfStore.Keys()...)
	return keys
}

func writeAndSync(path string, body []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// syncDir flushes a directory entry, so a rename survives a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	// Some filesystems refuse to sync a directory. That is not a reason to fail
	// a rewrite that has otherwise succeeded, so it is reported by returning
	// nil here rather than undoing the swap.
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
	if aof.file == nil || rewriteInProgress || config.AOFAutoRewritePercentage <= 0 {
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
	if err := RewriteAOF(); err != nil {
		aofLog("automatic rewrite failed: %v", err)
	}
}

// AOFStats reports what INFO needs to say about the log.
func AOFStats() (baseSize, currentSize int64, rewrites int, keys int) {
	if aof.file == nil {
		return 0, 0, 0, 0
	}
	return aof.baseSize, aof.baseSize + aof.written, aof.rewrites, aof.lastKeys
}

var _ = data_structure.TotalKeys
