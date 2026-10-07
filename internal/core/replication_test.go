package core

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReplicationCanonicalImagesAndOrdering(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	oldExpiry, oldEviction := e.space.SuspendExpiry, e.space.SuspendEviction
	defer func() {
		e.CloseAOF()
		e.space.SuspendExpiry = oldExpiry
		e.space.SuspendEviction = oldEviction
	}()
	e.resetStores()
	reconfigure(t, e, func(o *Options) { o.ReplicationFeed = true })
	require.NoError(t, e.OpenAOF(filepath.Join(t.TempDir(), "primary")))
	require.NoError(t, e.InitReplication())
	fillOneOfEverythingOn(t, e)
	runOn(t, e, "HSET", "hash", "field", "value")
	runOn(t, e, "RPUSH", "list", "a", "b")
	runOn(t, e, "PEXPIRE", "list", "60000")
	pull := func(epoch, offset string) ReplicationFrame {
		reply := runOn(t, e, "KEEL.REPL.PULL", epoch, offset)
		encoded, ok := reply.(string)
		require.True(t, ok)
		var f ReplicationFrame
		require.NoError(t, json.Unmarshal([]byte(encoded), &f))
		return f
	}
	first := pull("", "0")
	require.True(t, first.Full)
	runOn(t, e, "MORRIS.INCRBY", "mor", "hits", "10000")
	runOn(t, e, "CF.ADD", "cf", "second")
	runOn(t, e, "LPOP", "list")
	runOn(t, e, "DEL", "str")
	second := pull(first.Epoch, "1")
	require.False(t, second.Full)
	expected := snapshotEverythingOn(t, e)
	morris, _ := e.dumpKey("mor")
	cuckoo, _ := e.dumpKey("cf")
	require.NoError(t, e.CloseAOF())
	e.resetStores()
	reconfigure(t, e, func(o *Options) { o.ReplicationFeed, o.ReplicaOf = false, "test-primary:6379" })
	require.NoError(t, e.InitReplication())
	require.NoError(t, e.OpenAOF(filepath.Join(t.TempDir(), "replica")))
	require.Error(t, e.ApplyReplication(second), "delta requires initial snapshot")
	require.NoError(t, e.ApplyReplication(first))
	corrupt := second
	corrupt.Checksum = "bad"
	require.Error(t, e.ApplyReplication(corrupt))
	emptyAdvance := second
	emptyAdvance.Body = nil
	emptyAdvance.Checksum = frameChecksum(emptyAdvance)
	require.Error(t, e.ApplyReplication(emptyAdvance), "an empty delta cannot advance the cursor")
	require.NoError(t, e.ApplyReplication(second))
	require.Equal(t, expected, snapshotEverythingOn(t, e))
	got, _ := e.dumpKey("mor")
	require.Equal(t, morris, got)
	got, _ = e.dumpKey("cf")
	require.Equal(t, cuckoo, got)
	require.Contains(t, string(rawReplyOn(t, e, "SET", "forbidden", "v")), "READONLY")
	require.Nil(t, e.dictStore.Peek("forbidden"))
	require.Error(t, e.ApplyReplication(firstDeltaWithGap(second)))
	e.replicaUpdated = time.Now().Add(-6 * time.Second)
	require.Contains(t, string(rawReplyOn(t, e, "GET", "num")), "MASTERDOWN")
}
func firstDeltaWithGap(f ReplicationFrame) ReplicationFrame {
	f.From = f.To + 1
	f.To = f.From
	f.Checksum = frameChecksum(f)
	return f
}

func TestReplicationRejectsMalformedSnapshotBeforeMutation(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "SET", "sentinel", "present")
	oldReady := e.replicaReady
	e.replicaReady = false
	defer func() { e.replicaReady = oldReady }()
	reconfigure(t, e, func(o *Options) { o.ReplicaOf = "test:1" })
	f := ReplicationFrame{Version: 1, Epoch: "0123456789abcdef0123456789abcdef", Full: true, Body: []byte("*1\r\n$7\r\nFLUSHDB\r\n*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$999999999\r\n")}
	f.Checksum = frameChecksum(f)
	require.ErrorContains(t, e.ApplyReplication(f), "malformed replication command")
	require.Equal(t, "present", e.dictStore.Peek("sentinel").Value)
}

func TestReplicationHistoryAndDirtyOverflowRequireFullSync(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	defer e.CloseAOF()
	e.resetStores()
	reconfigure(t, e, func(o *Options) { o.ReplicationFeed = true })
	require.NoError(t, e.OpenAOF(filepath.Join(t.TempDir(), "primary")))
	require.NoError(t, e.InitReplication())
	epoch := e.replication.epoch
	for i := 0; i < 1030; i++ {
		runOn(t, e, "SET", "key", "value")
		require.NoError(t, e.sealReplication())
	}
	require.LessOrEqual(t, len(e.replication.history), 1024)
	var frame ReplicationFrame
	require.NoError(t, json.Unmarshal([]byte(runOn(t, e, "KEEL.REPL.PULL", epoch, "0").(string)), &frame))
	require.True(t, frame.Full)
	e.replication.dirtyBytes = replicationLimit
	e.noteReplicationDirty("new-key")
	require.True(t, e.replication.invalidated)
	require.NoError(t, json.Unmarshal([]byte(runOn(t, e, "KEEL.REPL.PULL", epoch, "1030").(string)), &frame))
	require.True(t, frame.Full)
	require.NotEqual(t, epoch, frame.Epoch)
}

func TestReplicationApplyFailureDisablesReadsUntilFullSync(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	oldExpiry, oldEviction := e.space.SuspendExpiry, e.space.SuspendEviction
	defer func() {
		e.space.SuspendExpiry, e.space.SuspendEviction = oldExpiry, oldEviction
	}()
	reconfigure(t, e, func(o *Options) { o.ReplicaOf = "test:1" })
	require.NoError(t, e.InitReplication())
	full := ReplicationFrame{Version: 1, Epoch: "0123456789abcdef0123456789abcdef", Full: true, To: 1}
	full.Body = appendCommand(nil, "FLUSHDB")
	full.Body = appendCommand(full.Body, "SET", "k", "original")
	full.Checksum = frameChecksum(full)
	require.NoError(t, e.ApplyReplication(full))
	require.Equal(t, "original", runOn(t, e, "GET", "k"))
	updated := e.replicaUpdated
	broken := ReplicationFrame{Version: 1, Epoch: full.Epoch, From: 1, To: 2}
	broken.Body = appendCommand(nil, "DEL", "k")
	broken.Body = appendCommand(broken.Body, "SET", "k") // valid RESP, invalid command arity
	broken.Checksum = frameChecksum(broken)
	require.ErrorContains(t, e.ApplyReplication(broken), "replication apply")
	require.Nil(t, e.dictStore.Peek("k"), "the first command applied before the second failed")
	require.False(t, e.replicaReady)
	require.False(t, e.replicaApplying)
	require.Equal(t, uint64(1), e.replicaOffset)
	require.Equal(t, updated, e.replicaUpdated)
	require.Contains(t, string(rawReplyOn(t, e, "GET", "k")), "MASTERDOWN")
	broken.Body = appendCommand(nil, "SET", "k", "replacement")
	broken.Checksum = frameChecksum(broken)
	require.ErrorContains(t, e.ApplyReplication(broken), "replication offset gap", "delta cannot repair unknown partial state")
	full.From = 1
	full.Checksum = frameChecksum(full)
	require.NoError(t, e.ApplyReplication(full))
	require.Equal(t, "original", runOn(t, e, "GET", "k"))
}
