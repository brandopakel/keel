package core

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func setupReplicationV2(t *testing.T) {
	t.Helper()
	setupReplicationV2On(t, defaultEngine)
}

// setupReplicationV2On is setupReplicationV2 on e.
func setupReplicationV2On(t *testing.T, e *Engine) {
	t.Helper()
	// Registered first, so it runs last, once the options are back: the
	// stream and the replica start afresh in the role those give, so that
	// nothing this test fed or applied stays on e.
	t.Cleanup(func() { require.NoError(t, e.InitReplication()) })
	// The options are put back next, after the replica and the stream are.
	withOptionsOn(t, e, func(o *Options) {
		o.ReplicationProtocol, o.Fsync, o.ReplicationFeed, o.ReplicaOf = 2, FsyncNever, true, ""
	})
	oldExpiry, oldEviction := e.space.SuspendExpiry, e.space.SuspendEviction
	t.Cleanup(func() {
		// Closing cancels a rewrite still running, as a server's shutdown
		// does, without counting it as a failed one. A failure would outlive
		// the keyspace and the test, and the default engine would report it
		// to every test after this one.
		e.CloseAOF()
		e.resetReplicationV2()
		e.resetReplica()
		e.space.SuspendExpiry, e.space.SuspendEviction = oldExpiry, oldEviction
	})
	e.resetStores()
	require.NoError(t, e.OpenAOF(filepath.Join(t.TempDir(), "primary.aof")))
	require.NoError(t, e.InitReplication())
}

func pullV2(t *testing.T, epoch string, offset uint64, snapshot string, part uint64) ReplicationFrame {
	t.Helper()
	return pullV2On(t, defaultEngine, epoch, offset, snapshot, part)
}

// pullV2On is pullV2 on e.
func pullV2On(t *testing.T, e *Engine, epoch string, offset uint64, snapshot string, part uint64) ReplicationFrame {
	t.Helper()
	// A pull carries the caller's term. These tests are a single node acting as
	// both ends, so it sends the term it already holds.
	reply := runOn(t, e, "KEEL.REPL.PULL2", epoch, strconv.FormatUint(offset, 10), snapshot,
		strconv.FormatUint(part, 10), strconv.FormatUint(e.failover.term, 10))
	encoded, ok := reply.(string)
	require.True(t, ok, "reply: %v", reply)
	var frame ReplicationFrame
	require.NoError(t, json.Unmarshal([]byte(encoded), &frame))
	require.Equal(t, frameChecksum(frame), frame.Checksum)
	require.LessOrEqual(t, len(frame.Body), replicationChunkBytes)
	return frame
}

func snapshotV2(t *testing.T) []ReplicationFrame {
	t.Helper()
	return snapshotV2On(t, defaultEngine)
}

// snapshotV2On is snapshotV2 on e.
func snapshotV2On(t *testing.T, e *Engine) []ReplicationFrame {
	t.Helper()
	first := pullV2On(t, e, "", 0, "", 0)
	require.True(t, first.Pending)
	for n := 0; e.RewriteActive() && n < 100000; n++ {
		require.NoError(t, e.FlushAOF())
		waitForRewriteSyncOn(t, e)
	}
	require.False(t, e.RewriteActive())
	var frames []ReplicationFrame
	for id, part := "", uint64(0); ; {
		f := pullV2On(t, e, "", 0, id, part)
		require.True(t, f.Full)
		require.False(t, f.Pending)
		frames = append(frames, f)
		if f.SnapshotDone {
			return frames
		}
		id, part = f.SnapshotID, part+uint64(len(f.Body))
	}
}

// becomeReplicaV2On turns e into a protocol 2 replica with a log of its own,
// and returns the log's path.
func becomeReplicaV2On(t *testing.T, e *Engine) string {
	t.Helper()
	require.NoError(t, e.CloseAOF())
	e.resetStores()
	withOptionsOn(t, e, func(o *Options) { o.ReplicationFeed, o.ReplicaOf = false, "primary.test:6379" })
	path := filepath.Join(t.TempDir(), "replica.aof")
	require.NoError(t, e.OpenAOF(path))
	require.NoError(t, e.InitReplication())
	return path
}

func signedV2(f ReplicationFrame) ReplicationFrame {
	f.Checksum = frameChecksum(f)
	return f
}

func TestReplicationV2LargeSnapshotOperationsAndFrozenFile(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	setupReplicationV2On(t, e)
	fillOneOfEverythingOn(t, e)
	for n := 0; n < 160; n++ {
		runOn(t, e, "SET", "bulk:"+strconv.Itoa(n), strings.Repeat(strconv.Itoa(n%10), 64<<10))
	}
	for n := 0; n < 3000; n++ {
		runOn(t, e, "RPUSH", "large-list", strings.Repeat("l", 64))
		runOn(t, e, "HSET", "large-hash", strconv.Itoa(n), "value")
		runOn(t, e, "ZADD", "large-zset", strconv.Itoa(n), strconv.Itoa(n))
	}
	frames := snapshotV2On(t, e)
	require.Greater(t, frames[0].SnapshotBytes, uint64(8<<20))
	require.Greater(t, len(frames), 32)
	base, epoch := frames[0].To, frames[0].Epoch
	runOn(t, e, "LSET", "large-list", "1000", "changed")
	runOn(t, e, "HINCRBY", "large-hash", "counter", "7")
	runOn(t, e, "ZINCRBY", "large-zset", "3", "19")
	delta := pullV2On(t, e, epoch, base, "", 0)
	require.False(t, delta.Full)
	require.Less(t, len(delta.Body), 256, "large collections must send operations, not whole key images")
	runOn(t, e, "MORRIS.INCRBY", "mor", "hits", "10000")
	runOn(t, e, "CF.ADD", "cf", "second")
	runOn(t, e, "PFADD", "hll", "another")
	opaque := pullV2On(t, e, epoch, delta.To, "", 0)
	morris, _ := e.dumpKey("mor")
	cuckoo, _ := e.dumpKey("cf")
	hll, _ := e.dumpKey("hll")
	want := snapshotEverythingOn(t, e)
	// Replacing the AOF inode must not change chunks from the frozen snapshot.
	require.NoError(t, e.StartRewrite())
	for e.RewriteActive() {
		require.NoError(t, e.FlushAOF())
		waitForRewriteSyncOn(t, e)
	}
	got := pullV2On(t, e, epoch, 0, frames[0].SnapshotID, 0)
	require.Equal(t, frames[0], got)
	path := becomeReplicaV2On(t, e)
	for _, frame := range frames {
		require.NoError(t, e.ApplyReplication(frame))
		require.False(t, e.replicaReady, "even a complete snapshot must wait for catch-up")
	}
	require.NoError(t, e.ApplyReplication(delta))
	require.NoError(t, e.ApplyReplication(opaque))
	require.True(t, e.replicaReady)
	require.Equal(t, want, snapshotEverythingOn(t, e))
	for key, want := range map[string][]byte{"mor": morris, "cf": cuckoo, "hll": hll} {
		got, ok := e.dumpKey(key)
		require.True(t, ok)
		require.Equal(t, want, got)
	}
	assertCheckpointDigestOn(t, e, path)
}

// assertCheckpointDigestOn requires e's log digest to be that of the file at
// path.
func assertCheckpointDigestOn(t *testing.T, e *Engine, path string) {
	t.Helper()
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	sum := sha256.Sum256(body)
	require.Equal(t, hex.EncodeToString(sum[:]), e.currentAOFDigest())
	require.Equal(t, int64(len(body)), e.aof.digestBytes)
}

func TestReplicationV2CheckpointRestartExpiryAndRewrite(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	setupReplicationV2On(t, e)
	runOn(t, e, "SET", "counter", "9")
	runOn(t, e, "PEXPIREAT", "counter", strconv.FormatInt(time.Now().Add(time.Minute).UnixMilli(), 10))
	frames := snapshotV2On(t, e)
	base, epoch := frames[0].To, frames[0].Epoch
	path := becomeReplicaV2On(t, e)
	for _, f := range frames {
		require.NoError(t, e.ApplyReplication(f))
	}
	// The primary's earlier expiry decision arrives late, followed by INCR.
	// A replica must preserve state until the primary sends DEL.
	body := appendCommand(nil, "PEXPIREAT", "counter", "1")
	body = appendCommand(body, "INCR", "counter")
	f := signedV2(ReplicationFrame{Version: 2, Epoch: epoch, From: base, To: base + uint64(len(body)), Body: body, CaughtUp: true})
	require.NoError(t, e.ApplyReplication(f))
	require.Equal(t, "10", runOn(t, e, "GET", "counter"))
	assertCheckpointDigestOn(t, e, path)
	for n := 0; n < 2; n++ {
		require.NoError(t, e.CloseAOF())
		e.resetStores()
		_, err := e.LoadAOF(path)
		require.NoError(t, err)
		require.NoError(t, e.OpenAOF(path))
		require.NoError(t, e.InitReplication())
		gotEpoch, gotOffset := e.ReplicaResumeCursor()
		require.Equal(t, epoch, gotEpoch)
		require.Equal(t, f.To, gotOffset)
		require.True(t, e.replicaV2.resumed)
		require.False(t, e.replicaReady)
		require.NoError(t, e.ApplyReplication(signedV2(ReplicationFrame{Version: 2, Epoch: epoch, From: f.To, To: f.To, CaughtUp: true})))
		require.Equal(t, "10", runOn(t, e, "GET", "counter"))
		require.NoError(t, e.StartRewrite())
		for e.RewriteActive() {
			require.NoError(t, e.FlushAOF())
			waitForRewriteSyncOn(t, e)
		}
		require.NoError(t, e.saveReplicaCheckpoint())
		assertCheckpointDigestOn(t, e, path)
	}
}

func TestReplicationV2CheckpointInvalidFilesFallBack(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	for _, damage := range []string{"suffix", "changed", "truncated", "oversized-metadata", "bad-json", "different-primary", "missing"} {
		t.Run(damage, func(t *testing.T) {
			setupReplicationV2On(t, e)
			runOn(t, e, "SET", "k", "original")
			frames := snapshotV2On(t, e)
			path := becomeReplicaV2On(t, e)
			for _, f := range frames {
				require.NoError(t, e.ApplyReplication(f))
			}
			require.NoError(t, e.ApplyReplication(signedV2(ReplicationFrame{Version: 2, Epoch: frames[0].Epoch, From: frames[0].To, To: frames[0].To, CaughtUp: true})))
			require.NoError(t, e.CloseAOF())
			body, err := os.ReadFile(path)
			require.NoError(t, err)
			switch damage {
			case "suffix":
				body = appendCommand(body, "INCR", "new")
			case "changed":
				body = bytes.ReplaceAll(body, []byte("original"), []byte("modified"))
			case "truncated":
				body = nil
			case "oversized-metadata":
				require.NoError(t, os.WriteFile(path+".replica-checkpoint", bytes.Repeat([]byte(" "), 1<<20), 0600))
			case "bad-json":
				require.NoError(t, os.WriteFile(path+".replica-checkpoint", []byte("{"), 0600))
			case "different-primary":
				reconfigure(t, e, func(o *Options) { o.ReplicaOf = "different.test:6379" })
			case "missing":
				require.NoError(t, os.Remove(path+".replica-checkpoint"))
			}
			require.NoError(t, os.WriteFile(path, body, 0600))
			e.resetStores()
			_, err = e.LoadAOF(path)
			require.NoError(t, err)
			require.NoError(t, e.OpenAOF(path))
			require.NoError(t, e.InitReplication())
			epoch, offset := e.ReplicaResumeCursor()
			require.Empty(t, epoch)
			require.Zero(t, offset)
			require.False(t, e.replicaReady)
		})
	}
}

func TestReplicationV2CheckpointFaultsGateReads(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	for _, fault := range []string{"aof-sync", "metadata-sync", "rename", "directory-sync"} {
		t.Run(fault, func(t *testing.T) {
			setupReplicationV2On(t, e)
			runOn(t, e, "SET", "k", "v")
			frames := snapshotV2On(t, e)
			becomeReplicaV2On(t, e)
			for _, f := range frames {
				require.NoError(t, e.ApplyReplication(f))
			}
			oldSync, oldCPSync, oldRename, oldDir := e.aofSync, e.checkpointSync, e.checkpointRename, e.checkpointSyncDir
			defer func() {
				e.aofSync, e.checkpointSync, e.checkpointRename, e.checkpointSyncDir = oldSync, oldCPSync, oldRename, oldDir
			}()
			failure := errors.New("injected checkpoint failure")
			switch fault {
			case "aof-sync":
				e.aofSync = func(*os.File) error { return failure }
			case "metadata-sync":
				e.checkpointSync = func(*os.File) error { return failure }
			case "rename":
				e.checkpointRename = func(string, string) error { return failure }
			case "directory-sync":
				e.checkpointSyncDir = func(string) error { return failure }
			}
			err := e.ApplyReplication(signedV2(ReplicationFrame{Version: 2, Epoch: frames[0].Epoch, From: frames[0].To, To: frames[0].To, CaughtUp: true}))
			require.ErrorIs(t, err, failure)
			require.False(t, e.replicaReady)
			require.False(t, e.replicaV2.trusted)
		})
	}
}

func TestReplicationV2RejectsChunkGapsCorruptionAndIncompleteCatchup(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	for _, damage := range []string{"checksum", "version", "gap", "identity", "premature-end", "operation-gap", "operation-incomplete"} {
		t.Run(damage, func(t *testing.T) {
			setupReplicationV2On(t, e)
			runOn(t, e, "SET", "large", strings.Repeat("v", 600<<10))
			frames := snapshotV2On(t, e)
			becomeReplicaV2On(t, e)
			require.NoError(t, e.ApplyReplication(frames[0]))
			broken := frames[1]
			switch damage {
			case "checksum":
				broken.Body = bytes.Clone(broken.Body)
				broken.Body[0] ^= 1
			case "version":
				broken.Version = 1
			case "gap":
				broken.SnapshotOffset++
			case "identity":
				broken.SnapshotID = strings.Repeat("a", 32)
			case "premature-end":
				broken.SnapshotDone = true
				broken.SnapshotBytes = broken.SnapshotOffset + uint64(len(broken.Body))
			default:
				for _, f := range frames[1:] {
					require.NoError(t, e.ApplyReplication(f))
				}
				broken = ReplicationFrame{Version: 2, Epoch: frames[0].Epoch, From: frames[0].To, To: frames[0].To, CaughtUp: true}
				if damage == "operation-gap" {
					broken.From++
					broken.To++
				} else {
					broken.Body = []byte("*3\r\n$3\r\nSET\r\n")
					broken.To += uint64(len(broken.Body))
				}
			}
			if damage != "checksum" {
				broken = signedV2(broken)
			}
			require.Error(t, e.ApplyReplication(broken))
			require.False(t, e.replicaReady)
			require.False(t, e.replicaV2.trusted)
		})
	}
}

func TestReplicationV2HistoryOverrunAndProtocolMismatch(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	setupReplicationV2On(t, e)
	frames := snapshotV2On(t, e)
	epoch, base, id := frames[0].Epoch, frames[0].To, frames[0].SnapshotID
	for n := 0; n < 4100; n++ {
		runOn(t, e, "INCR", "counter")
	}
	require.True(t, e.historyV2Contains(base), "small operations must use the byte budget rather than a per-command entry limit")
	for n := 0; n < 70; n++ {
		runOn(t, e, "SET", "history-fill", strings.Repeat("v", replicationChunkBytes))
	}
	require.LessOrEqual(t, len(e.replicationV2.history), 4096)
	require.LessOrEqual(t, e.replicationV2.bytes, replicationHistoryLimit)
	require.False(t, e.historyV2Contains(base))
	f := pullV2On(t, e, epoch, base, id, 0)
	require.True(t, f.Pending)
	f = pullV2On(t, e, epoch, base, "", 0)
	require.True(t, f.Pending)
	require.True(t, e.RewriteActive())
	require.Contains(t, string(rawReplyOn(t, e, "KEEL.REPL.PULL", "", "0")), "protocol 1 is disabled")
	reconfigure(t, e, func(o *Options) { o.ReplicationProtocol = 1 })
	require.Contains(t, string(rawReplyOn(t, e, "KEEL.REPL.PULL2", "", "0", "", "0")), "protocol 2 is disabled")
	reconfigure(t, e, func(o *Options) { o.ReplicationProtocol = 2 })
	e.invalidateReplicationV2()
	require.NotEqual(t, epoch, e.replication.epoch)
}
