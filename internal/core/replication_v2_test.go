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

	"github.com/brandopakel/keel/internal/config"
	"github.com/brandopakel/keel/internal/data_structure"
	"github.com/stretchr/testify/require"
)

func setupReplicationV2(t *testing.T) {
	t.Helper()
	oldFeed, oldReplica, oldProtocol, oldPolicy := config.ReplicationFeed, config.ReplicaOf, config.ReplicationProtocol, config.AOFFsync
	oldExpiry, oldEviction := data_structure.SuspendExpiry, data_structure.SuspendEviction
	t.Cleanup(func() {
		if RewriteActive() {
			abortRewrite(errors.New("test cleanup"))
		}
		CloseAOF()
		resetReplicationV2()
		config.ReplicationFeed, config.ReplicaOf, config.ReplicationProtocol, config.AOFFsync = oldFeed, oldReplica, oldProtocol, oldPolicy
		data_structure.SuspendExpiry, data_structure.SuspendEviction = oldExpiry, oldEviction
	})
	ResetStores()
	config.ReplicationProtocol, config.AOFFsync = 2, config.FsyncNever
	config.ReplicationFeed, config.ReplicaOf = true, ""
	require.NoError(t, OpenAOF(filepath.Join(t.TempDir(), "primary.aof")))
	require.NoError(t, InitReplication())
}

func pullV2(t *testing.T, epoch string, offset uint64, snapshot string, part uint64) ReplicationFrame {
	t.Helper()
	reply := run(t, "KEEL.REPL.PULL2", epoch, strconv.FormatUint(offset, 10), snapshot, strconv.FormatUint(part, 10))
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
	first := pullV2(t, "", 0, "", 0)
	require.True(t, first.Pending)
	for n := 0; RewriteActive() && n < 100000; n++ {
		require.NoError(t, FlushAOF())
	}
	require.False(t, RewriteActive())
	var frames []ReplicationFrame
	for id, part := "", uint64(0); ; {
		f := pullV2(t, "", 0, id, part)
		require.True(t, f.Full)
		require.False(t, f.Pending)
		frames = append(frames, f)
		if f.SnapshotDone {
			return frames
		}
		id, part = f.SnapshotID, part+uint64(len(f.Body))
	}
}

func becomeReplicaV2(t *testing.T) string {
	t.Helper()
	require.NoError(t, CloseAOF())
	ResetStores()
	config.ReplicationFeed, config.ReplicaOf = false, "primary.test:6379"
	path := filepath.Join(t.TempDir(), "replica.aof")
	require.NoError(t, OpenAOF(path))
	require.NoError(t, InitReplication())
	return path
}

func signedV2(f ReplicationFrame) ReplicationFrame {
	f.Checksum = frameChecksum(f)
	return f
}

func TestReplicationV2LargeSnapshotOperationsAndFrozenFile(t *testing.T) {
	setupReplicationV2(t)
	fillOneOfEverything(t)
	for n := 0; n < 160; n++ {
		run(t, "SET", "bulk:"+strconv.Itoa(n), strings.Repeat(strconv.Itoa(n%10), 64<<10))
	}
	for n := 0; n < 3000; n++ {
		run(t, "RPUSH", "large-list", strings.Repeat("l", 64))
		run(t, "HSET", "large-hash", strconv.Itoa(n), "value")
		run(t, "ZADD", "large-zset", strconv.Itoa(n), strconv.Itoa(n))
	}
	frames := snapshotV2(t)
	require.Greater(t, frames[0].SnapshotBytes, uint64(8<<20))
	require.Greater(t, len(frames), 32)
	base, epoch := frames[0].To, frames[0].Epoch
	run(t, "LSET", "large-list", "1000", "changed")
	run(t, "HINCRBY", "large-hash", "counter", "7")
	run(t, "ZINCRBY", "large-zset", "3", "19")
	delta := pullV2(t, epoch, base, "", 0)
	require.False(t, delta.Full)
	require.Less(t, len(delta.Body), 256, "large collections must send operations, not whole key images")
	run(t, "MORRIS.INCRBY", "mor", "hits", "10000")
	run(t, "CF.ADD", "cf", "second")
	run(t, "PFADD", "hll", "another")
	opaque := pullV2(t, epoch, delta.To, "", 0)
	morris, _ := dumpKey("mor")
	cuckoo, _ := dumpKey("cf")
	hll, _ := dumpKey("hll")
	want := snapshotEverything(t)
	// Replacing the AOF inode must not change chunks from the frozen snapshot.
	require.NoError(t, StartRewrite())
	for RewriteActive() {
		require.NoError(t, FlushAOF())
	}
	got := pullV2(t, epoch, 0, frames[0].SnapshotID, 0)
	require.Equal(t, frames[0], got)
	path := becomeReplicaV2(t)
	for _, frame := range frames {
		require.NoError(t, ApplyReplication(frame))
		require.False(t, replicaReady, "even a complete snapshot must wait for catch-up")
	}
	require.NoError(t, ApplyReplication(delta))
	require.NoError(t, ApplyReplication(opaque))
	require.True(t, replicaReady)
	require.Equal(t, want, snapshotEverything(t))
	for key, want := range map[string][]byte{"mor": morris, "cf": cuckoo, "hll": hll} {
		got, ok := dumpKey(key)
		require.True(t, ok)
		require.Equal(t, want, got)
	}
	assertCheckpointDigest(t, path)
}

func assertCheckpointDigest(t *testing.T, path string) {
	t.Helper()
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	sum := sha256.Sum256(body)
	require.Equal(t, hex.EncodeToString(sum[:]), currentAOFDigest())
	require.Equal(t, int64(len(body)), aof.digestBytes)
}

func TestReplicationV2CheckpointRestartExpiryAndRewrite(t *testing.T) {
	setupReplicationV2(t)
	run(t, "SET", "counter", "9")
	run(t, "PEXPIREAT", "counter", strconv.FormatInt(time.Now().Add(time.Minute).UnixMilli(), 10))
	frames := snapshotV2(t)
	base, epoch := frames[0].To, frames[0].Epoch
	path := becomeReplicaV2(t)
	for _, f := range frames {
		require.NoError(t, ApplyReplication(f))
	}
	// The primary's earlier expiry decision arrives late, followed by INCR.
	// A replica must preserve state until the primary sends DEL.
	body := appendCommand(nil, "PEXPIREAT", "counter", "1")
	body = appendCommand(body, "INCR", "counter")
	f := signedV2(ReplicationFrame{Version: 2, Epoch: epoch, From: base, To: base + uint64(len(body)), Body: body, CaughtUp: true})
	require.NoError(t, ApplyReplication(f))
	require.Equal(t, "10", run(t, "GET", "counter"))
	assertCheckpointDigest(t, path)
	for n := 0; n < 2; n++ {
		require.NoError(t, CloseAOF())
		ResetStores()
		_, err := LoadAOF(path)
		require.NoError(t, err)
		require.NoError(t, OpenAOF(path))
		require.NoError(t, InitReplication())
		gotEpoch, gotOffset := ReplicaResumeCursor()
		require.Equal(t, epoch, gotEpoch)
		require.Equal(t, f.To, gotOffset)
		require.True(t, replicaV2.resumed)
		require.False(t, replicaReady)
		require.NoError(t, ApplyReplication(signedV2(ReplicationFrame{Version: 2, Epoch: epoch, From: f.To, To: f.To, CaughtUp: true})))
		require.Equal(t, "10", run(t, "GET", "counter"))
		require.NoError(t, StartRewrite())
		for RewriteActive() {
			require.NoError(t, FlushAOF())
		}
		require.NoError(t, saveReplicaCheckpoint())
		assertCheckpointDigest(t, path)
	}
}

func TestReplicationV2CheckpointInvalidFilesFallBack(t *testing.T) {
	for _, damage := range []string{"suffix", "changed", "truncated", "oversized-metadata", "bad-json", "different-primary", "missing"} {
		t.Run(damage, func(t *testing.T) {
			setupReplicationV2(t)
			run(t, "SET", "k", "original")
			frames := snapshotV2(t)
			path := becomeReplicaV2(t)
			for _, f := range frames {
				require.NoError(t, ApplyReplication(f))
			}
			require.NoError(t, ApplyReplication(signedV2(ReplicationFrame{Version: 2, Epoch: frames[0].Epoch, From: frames[0].To, To: frames[0].To, CaughtUp: true})))
			require.NoError(t, CloseAOF())
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
				config.ReplicaOf = "different.test:6379"
			case "missing":
				require.NoError(t, os.Remove(path+".replica-checkpoint"))
			}
			require.NoError(t, os.WriteFile(path, body, 0600))
			ResetStores()
			_, err = LoadAOF(path)
			require.NoError(t, err)
			require.NoError(t, OpenAOF(path))
			require.NoError(t, InitReplication())
			epoch, offset := ReplicaResumeCursor()
			require.Empty(t, epoch)
			require.Zero(t, offset)
			require.False(t, replicaReady)
		})
	}
}

func TestReplicationV2CheckpointFaultsGateReads(t *testing.T) {
	for _, fault := range []string{"aof-sync", "metadata-sync", "rename", "directory-sync"} {
		t.Run(fault, func(t *testing.T) {
			setupReplicationV2(t)
			run(t, "SET", "k", "v")
			frames := snapshotV2(t)
			becomeReplicaV2(t)
			for _, f := range frames {
				require.NoError(t, ApplyReplication(f))
			}
			oldSync, oldCPSync, oldRename, oldDir := aofSync, checkpointSync, checkpointRename, checkpointSyncDir
			defer func() {
				aofSync, checkpointSync, checkpointRename, checkpointSyncDir = oldSync, oldCPSync, oldRename, oldDir
			}()
			failure := errors.New("injected checkpoint failure")
			switch fault {
			case "aof-sync":
				aofSync = func(*os.File) error { return failure }
			case "metadata-sync":
				checkpointSync = func(*os.File) error { return failure }
			case "rename":
				checkpointRename = func(string, string) error { return failure }
			case "directory-sync":
				checkpointSyncDir = func(string) error { return failure }
			}
			err := ApplyReplication(signedV2(ReplicationFrame{Version: 2, Epoch: frames[0].Epoch, From: frames[0].To, To: frames[0].To, CaughtUp: true}))
			require.ErrorIs(t, err, failure)
			require.False(t, replicaReady)
			require.False(t, replicaV2.trusted)
		})
	}
}

func TestReplicationV2RejectsChunkGapsCorruptionAndIncompleteCatchup(t *testing.T) {
	for _, damage := range []string{"checksum", "version", "gap", "identity", "premature-end", "operation-gap", "operation-incomplete"} {
		t.Run(damage, func(t *testing.T) {
			setupReplicationV2(t)
			run(t, "SET", "large", strings.Repeat("v", 600<<10))
			frames := snapshotV2(t)
			becomeReplicaV2(t)
			require.NoError(t, ApplyReplication(frames[0]))
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
					require.NoError(t, ApplyReplication(f))
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
			require.Error(t, ApplyReplication(broken))
			require.False(t, replicaReady)
			require.False(t, replicaV2.trusted)
		})
	}
}

func TestReplicationV2HistoryOverrunAndProtocolMismatch(t *testing.T) {
	setupReplicationV2(t)
	frames := snapshotV2(t)
	epoch, base, id := frames[0].Epoch, frames[0].To, frames[0].SnapshotID
	for n := 0; n < 4100; n++ {
		run(t, "INCR", "counter")
	}
	require.True(t, historyV2Contains(base), "small operations must use the byte budget rather than a per-command entry limit")
	for n := 0; n < 70; n++ {
		run(t, "SET", "history-fill", strings.Repeat("v", replicationChunkBytes))
	}
	require.LessOrEqual(t, len(replicationV2.history), 4096)
	require.LessOrEqual(t, replicationV2.bytes, replicationHistoryLimit)
	require.False(t, historyV2Contains(base))
	f := pullV2(t, epoch, base, id, 0)
	require.True(t, f.Pending)
	f = pullV2(t, epoch, base, "", 0)
	require.True(t, f.Pending)
	require.True(t, RewriteActive())
	require.Contains(t, string(rawReply(t, "KEEL.REPL.PULL", "", "0")), "protocol 1 is disabled")
	config.ReplicationProtocol = 1
	require.Contains(t, string(rawReply(t, "KEEL.REPL.PULL2", "", "0", "", "0")), "protocol 2 is disabled")
	config.ReplicationProtocol = 2
	invalidateReplicationV2()
	require.NotEqual(t, epoch, replication.epoch)
}
