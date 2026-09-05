package core

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/brandopakel/keel/internal/config"
	"github.com/brandopakel/keel/internal/data_structure"
	"github.com/stretchr/testify/require"
)

func TestReplicationCanonicalImagesAndOrdering(t *testing.T) {
	oldFeed, oldReplica := config.ReplicationFeed, config.ReplicaOf
	oldExpiry, oldEviction := data_structure.SuspendExpiry, data_structure.SuspendEviction
	defer func() {
		CloseAOF()
		config.ReplicationFeed = oldFeed
		config.ReplicaOf = oldReplica
		data_structure.SuspendExpiry = oldExpiry
		data_structure.SuspendEviction = oldEviction
	}()
	ResetStores()
	config.ReplicationFeed = true
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
	morris, _ := dumpKey("mor")
	cuckoo, _ := dumpKey("cf")
	require.NoError(t, CloseAOF())
	ResetStores()
	config.ReplicationFeed = false
	config.ReplicaOf = "test-primary:6379"
	require.NoError(t, InitReplication())
	require.NoError(t, OpenAOF(filepath.Join(t.TempDir(), "replica")))
	require.Error(t, ApplyReplication(second), "delta requires initial snapshot")
	require.NoError(t, ApplyReplication(first))
	corrupt := second
	corrupt.Checksum = "bad"
	require.Error(t, ApplyReplication(corrupt))
	require.NoError(t, ApplyReplication(second))
	require.Equal(t, expected, snapshotEverything(t))
	got, _ := dumpKey("mor")
	require.Equal(t, morris, got)
	got, _ = dumpKey("cf")
	require.Equal(t, cuckoo, got)
	require.Contains(t, string(rawReply(t, "SET", "forbidden", "v")), "READONLY")
	require.Nil(t, dictStore.Peek("forbidden"))
	require.Error(t, ApplyReplication(firstDeltaWithGap(second)))
	replicaUpdated = time.Now().Add(-6 * time.Second)
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
	oldReplica := config.ReplicaOf
	defer func() { config.ReplicaOf = oldReplica }()
	config.ReplicaOf = "test:1"
	f := ReplicationFrame{Version: 1, Epoch: "0123456789abcdef0123456789abcdef", Full: true, Body: []byte("*1\r\n$7\r\nFLUSHDB\r\n*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$999999999\r\n")}
	f.Checksum = frameChecksum(f)
	require.Error(t, ApplyReplication(f))
}

func TestReplicationHistoryAndDirtyOverflowRequireFullSync(t *testing.T) {
	oldFeed := config.ReplicationFeed
	defer func() { CloseAOF(); config.ReplicationFeed = oldFeed }()
	ResetStores()
	config.ReplicationFeed = true
	require.NoError(t, OpenAOF(filepath.Join(t.TempDir(), "primary")))
	require.NoError(t, InitReplication())
	epoch := replication.epoch
	for i := 0; i < 1030; i++ {
		run(t, "SET", "key", "value")
		require.NoError(t, sealReplication())
	}
	require.LessOrEqual(t, len(replication.history), 1024)
	var frame ReplicationFrame
	require.NoError(t, json.Unmarshal([]byte(run(t, "KEEL.REPL.PULL", epoch, "0").(string)), &frame))
	require.True(t, frame.Full)
	replication.dirtyBytes = replicationLimit
	noteReplicationDirty("new-key")
	require.True(t, replication.invalidated)
	require.NoError(t, json.Unmarshal([]byte(run(t, "KEEL.REPL.PULL", epoch, "1030").(string)), &frame))
	require.True(t, frame.Full)
	require.NotEqual(t, epoch, frame.Epoch)
}
