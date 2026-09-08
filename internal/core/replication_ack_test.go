package core

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPrimaryLearnsReportedReceivedCursor(t *testing.T) {
	setupReplicationV2(t)
	t.Cleanup(resetReplicaAcknowledgement)

	offset, behind, age := ReplicationAcknowledged()
	require.Zero(t, offset)
	require.Zero(t, behind)
	require.EqualValues(t, -1, age, "nothing has acknowledged, which is not the same as being caught up")

	run(t, "SET", "k", "v")
	run(t, "SET", "k2", "v2")
	require.Greater(t, replicationV2.end, uint64(0), "the primary produced a stream to be behind")

	// A pull asking to resume from 0 says nothing has been received yet.
	pullV2(t, replication.epoch, 0, "", 0)
	offset, behind, age = ReplicationAcknowledged()
	require.Zero(t, offset)
	require.Equal(t, replicationV2.end, behind, "a replica at zero is behind by the whole stream")
	require.GreaterOrEqual(t, age, int64(0), "an acknowledgement arrived")

	// Asking to resume from the end reports receipt through that cursor.
	pullV2(t, replication.epoch, replicationV2.end, "", 0)
	offset, behind, _ = ReplicationAcknowledged()
	require.Equal(t, replicationV2.end, offset)
	require.Zero(t, behind, "a replica at the end has nothing outstanding")
}

// An offset only means something inside the epoch that produced it. A pull from
// another history names a position in a stream this primary never wrote.
func TestAcknowledgementIgnoresOffsetsFromAnotherEpoch(t *testing.T) {
	setupReplicationV2(t)
	t.Cleanup(resetReplicaAcknowledgement)
	run(t, "SET", "k", "v")

	pullV2(t, replication.epoch, replicationV2.end, "", 0)
	trusted, _, _ := ReplicationAcknowledged()
	require.Equal(t, replicationV2.end, trusted)

	// A stale or foreign epoch must not move it, in either direction.
	run(t, "KEEL.REPL.PULL2", "0123456789abcdef0123456789abcdef", "999999", "", "0",
		strconv.FormatUint(CurrentTerm(), 10))
	after, _, _ := ReplicationAcknowledged()
	require.Equal(t, trusted, after, "a foreign epoch's offset must not be believed")
}

// The recorded value is the furthest any replica has reached, not the nearest,
// so it cannot answer "do n replicas hold this write". This pins that down so
// nobody later reads it as a quorum signal.
func TestAcknowledgementTracksTheFurthestReplicaNotTheNearest(t *testing.T) {
	setupReplicationV2(t)
	t.Cleanup(resetReplicaAcknowledgement)
	run(t, "SET", "k", "v")
	end := replicationV2.end

	pullV2(t, replication.epoch, end, "", 0) // a replica that is caught up
	pullV2(t, replication.epoch, 0, "", 0)   // and one that is far behind

	offset, behind, _ := ReplicationAcknowledged()
	require.Equal(t, end, offset,
		"the furthest is what is recorded, which is why this is lag and not durability")
	require.Zero(t, behind)
}

func TestInfoReportsReplicationLag(t *testing.T) {
	setupReplicationV2(t)
	t.Cleanup(resetReplicaAcknowledgement)
	run(t, "SET", "k", "v")
	pullV2(t, replication.epoch, 0, "", 0)

	info, ok := run(t, "INFO", "replication").(string)
	require.True(t, ok)
	require.Contains(t, info, "replication_acked_offset:0")
	require.Contains(t, info, "replication_lag_bytes:"+strconv.FormatUint(replicationV2.end, 10))
	require.NotContains(t, info, "replication_acked_age_ms:-1", "an acknowledgement arrived")
}

func TestReplicaProgressIgnoresFutureCursor(t *testing.T) {
	setupReplicationV2(t)
	run(t, "SET", "k", "v")
	end := replicationV2.end
	pullV2(t, replication.epoch, end, "", 0)
	observed := time.Unix(100, 0)
	replicaAck.at = observed
	_ = cmdReplicationPullV2([]string{replication.epoch, strconv.FormatUint(end+1, 10), "", "0"})
	require.Equal(t, end, replicaAck.offset, "an offset beyond this stream is not progress")
	require.Equal(t, observed, replicaAck.at)
}

func TestReplicaProgressLowerCursorDoesNotRefreshBestAge(t *testing.T) {
	setupReplicationV2(t)
	run(t, "SET", "k", "v")
	pullV2(t, replication.epoch, replicationV2.end, "", 0)
	observed := time.Unix(100, 0)
	replicaAck.at = observed
	pullV2(t, replication.epoch, 0, "", 0)
	require.Equal(t, observed, replicaAck.at, "a lagging peer must not make the best cursor look fresh")
}

func TestReplicaProgressIgnoresMalformedAndSnapshotPulls(t *testing.T) {
	for _, cursor := range []struct{ name, snapshot, part string }{
		{"malformed delta", "", "1"},
		{"snapshot transfer", "stale-snapshot", "0"},
	} {
		t.Run(cursor.name, func(t *testing.T) {
			setupReplicationV2(t)
			run(t, "SET", "k", "v")
			pullV2(t, replication.epoch, replicationV2.end, "", 0)
			observed := time.Unix(100, 0)
			replicaAck.at = observed
			_ = cmdReplicationPullV2([]string{replication.epoch, strconv.FormatUint(replicationV2.end, 10), cursor.snapshot, cursor.part})
			require.Equal(t, observed, replicaAck.at, "only a validated delta cursor confirms stream progress")
		})
	}
}
