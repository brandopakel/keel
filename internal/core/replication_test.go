package core

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/brandopakel/keel/internal/data_structure"
	"github.com/stretchr/testify/require"
)

func TestReplicationCanonicalImagesAndOrdering(t *testing.T) {
	oldExpiry, oldEviction := data_structure.DefaultSpace.SuspendExpiry, data_structure.DefaultSpace.SuspendEviction
	defer func() {
		CloseAOF()
		data_structure.DefaultSpace.SuspendExpiry = oldExpiry
		data_structure.DefaultSpace.SuspendEviction = oldEviction
	}()
	ResetStores()
	// Registered before the options are changed, so it runs after they are
	// put back: the default engine ends as neither the primary nor the
	// replica it is in this test, with nothing of either left.
	t.Cleanup(func() { require.NoError(t, InitReplication()) })
	withOptions(t, func(o *Options) { o.ReplicationFeed = true })
	require.NoError(t, OpenAOF(filepath.Join(t.TempDir(), "primary")))
	require.NoError(t, InitReplication())
	fillOneOfEverything(t)
	run(t, "HSET", "hash", "field", "value")
	run(t, "RPUSH", "list", "a", "b")
	run(t, "PEXPIRE", "list", "60000")
	pull := func(epoch, offset string) ReplicationFrame {
		reply := run(t, "KEEL.REPL.PULL", epoch, offset)
		encoded, ok := reply.(string)
		require.True(t, ok)
		var f ReplicationFrame
		require.NoError(t, json.Unmarshal([]byte(encoded), &f))
		return f
	}
	first := pull("", "0")
	require.True(t, first.Full)
	run(t, "MORRIS.INCRBY", "mor", "hits", "10000")
	run(t, "CF.ADD", "cf", "second")
	run(t, "LPOP", "list")
	run(t, "DEL", "str")
	second := pull(first.Epoch, "1")
	require.False(t, second.Full)
	expected := snapshotEverything(t)
	morris, _ := defaultEngine.dumpKey("mor")
	cuckoo, _ := defaultEngine.dumpKey("cf")
	require.NoError(t, CloseAOF())
	ResetStores()
	withOptions(t, func(o *Options) { o.ReplicationFeed, o.ReplicaOf = false, "test-primary:6379" })
	require.NoError(t, InitReplication())
	require.NoError(t, OpenAOF(filepath.Join(t.TempDir(), "replica")))
	require.Error(t, ApplyReplication(second), "delta requires initial snapshot")
	require.NoError(t, ApplyReplication(first))
	corrupt := second
	corrupt.Checksum = "bad"
	require.Error(t, ApplyReplication(corrupt))
	emptyAdvance := second
	emptyAdvance.Body = nil
	emptyAdvance.Checksum = frameChecksum(emptyAdvance)
	require.Error(t, ApplyReplication(emptyAdvance), "an empty delta cannot advance the cursor")
	require.NoError(t, ApplyReplication(second))
	require.Equal(t, expected, snapshotEverything(t))
	got, _ := defaultEngine.dumpKey("mor")
	require.Equal(t, morris, got)
	got, _ = defaultEngine.dumpKey("cf")
	require.Equal(t, cuckoo, got)
	require.Contains(t, string(rawReply(t, "SET", "forbidden", "v")), "READONLY")
	require.Nil(t, defaultEngine.dictStore.Peek("forbidden"))
	require.Error(t, ApplyReplication(firstDeltaWithGap(second)))
	defaultEngine.replicaUpdated = time.Now().Add(-6 * time.Second)
	require.Contains(t, string(rawReply(t, "GET", "num")), "MASTERDOWN")
}
func firstDeltaWithGap(f ReplicationFrame) ReplicationFrame {
	f.From = f.To + 1
	f.To = f.From
	f.Checksum = frameChecksum(f)
	return f
}

func TestReplicationRejectsMalformedSnapshotBeforeMutation(t *testing.T) {
	ResetStores()
	run(t, "SET", "sentinel", "present")
	oldReady := defaultEngine.replicaReady
	defaultEngine.replicaReady = false
	defer func() { defaultEngine.replicaReady = oldReady }()
	withOptions(t, func(o *Options) { o.ReplicaOf = "test:1" })
	f := ReplicationFrame{Version: 1, Epoch: "0123456789abcdef0123456789abcdef", Full: true, Body: []byte("*1\r\n$7\r\nFLUSHDB\r\n*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$999999999\r\n")}
	f.Checksum = frameChecksum(f)
	require.ErrorContains(t, ApplyReplication(f), "malformed replication command")
	require.Equal(t, "present", defaultEngine.dictStore.Peek("sentinel").Value)
}

func TestReplicationHistoryAndDirtyOverflowRequireFullSync(t *testing.T) {
	defer CloseAOF()
	ResetStores()
	withOptions(t, func(o *Options) { o.ReplicationFeed = true })
	require.NoError(t, OpenAOF(filepath.Join(t.TempDir(), "primary")))
	require.NoError(t, InitReplication())
	epoch := defaultEngine.replication.epoch
	for i := 0; i < 1030; i++ {
		run(t, "SET", "key", "value")
		require.NoError(t, defaultEngine.sealReplication())
	}
	require.LessOrEqual(t, len(defaultEngine.replication.history), 1024)
	var frame ReplicationFrame
	require.NoError(t, json.Unmarshal([]byte(run(t, "KEEL.REPL.PULL", epoch, "0").(string)), &frame))
	require.True(t, frame.Full)
	defaultEngine.replication.dirtyBytes = replicationLimit
	defaultEngine.noteReplicationDirty("new-key")
	require.True(t, defaultEngine.replication.invalidated)
	require.NoError(t, json.Unmarshal([]byte(run(t, "KEEL.REPL.PULL", epoch, "1030").(string)), &frame))
	require.True(t, frame.Full)
	require.NotEqual(t, epoch, frame.Epoch)
}

func TestReplicationApplyFailureDisablesReadsUntilFullSync(t *testing.T) {
	ResetStores()
	oldExpiry, oldEviction := data_structure.DefaultSpace.SuspendExpiry, data_structure.DefaultSpace.SuspendEviction
	defer func() {
		data_structure.DefaultSpace.SuspendExpiry, data_structure.DefaultSpace.SuspendEviction = oldExpiry, oldEviction
	}()
	// Registered before the options are changed, so it runs after they are
	// put back: the default engine ends as no replica, with nothing applied.
	t.Cleanup(func() { require.NoError(t, InitReplication()) })
	withOptions(t, func(o *Options) { o.ReplicaOf = "test:1" })
	require.NoError(t, InitReplication())
	full := ReplicationFrame{Version: 1, Epoch: "0123456789abcdef0123456789abcdef", Full: true, To: 1}
	full.Body = appendCommand(nil, "FLUSHDB")
	full.Body = appendCommand(full.Body, "SET", "k", "original")
	full.Checksum = frameChecksum(full)
	require.NoError(t, ApplyReplication(full))
	require.Equal(t, "original", run(t, "GET", "k"))
	updated := defaultEngine.replicaUpdated
	broken := ReplicationFrame{Version: 1, Epoch: full.Epoch, From: 1, To: 2}
	broken.Body = appendCommand(nil, "DEL", "k")
	broken.Body = appendCommand(broken.Body, "SET", "k") // valid RESP, invalid command arity
	broken.Checksum = frameChecksum(broken)
	require.ErrorContains(t, ApplyReplication(broken), "replication apply")
	require.Nil(t, defaultEngine.dictStore.Peek("k"), "the first command applied before the second failed")
	require.False(t, defaultEngine.replicaReady)
	require.False(t, defaultEngine.replicaApplying)
	require.Equal(t, uint64(1), defaultEngine.replicaOffset)
	require.Equal(t, updated, defaultEngine.replicaUpdated)
	require.Contains(t, string(rawReply(t, "GET", "k")), "MASTERDOWN")
	broken.Body = appendCommand(nil, "SET", "k", "replacement")
	broken.Checksum = frameChecksum(broken)
	require.ErrorContains(t, ApplyReplication(broken), "replication offset gap", "delta cannot repair unknown partial state")
	full.From = 1
	full.Checksum = frameChecksum(full)
	require.NoError(t, ApplyReplication(full))
	require.Equal(t, "original", run(t, "GET", "k"))
}
