package core

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPrimaryLearnsReportedReceivedCursor(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	setupReplicationV2On(t, e)
	t.Cleanup(e.resetReplicaAcknowledgement)

	offset, behind, age := e.ReplicationAcknowledged()
	require.Zero(t, offset)
	require.Zero(t, behind)
	require.EqualValues(t, -1, age, "nothing has acknowledged, which is not the same as being caught up")

	runOn(t, e, "SET", "k", "v")
	runOn(t, e, "SET", "k2", "v2")
	require.Greater(t, e.replicationV2.end, uint64(0), "the primary produced a stream to be behind")

	// A pull asking to resume from 0 says nothing has been received yet.
	pullV2On(t, e, e.replication.epoch, 0, "", 0)
	offset, behind, age = e.ReplicationAcknowledged()
	require.Zero(t, offset)
	require.Equal(t, e.replicationV2.end, behind, "a replica at zero is behind by the whole stream")
	require.GreaterOrEqual(t, age, int64(0), "an acknowledgement arrived")

	// Asking to resume from the end reports receipt through that cursor.
	pullV2On(t, e, e.replication.epoch, e.replicationV2.end, "", 0)
	offset, behind, _ = e.ReplicationAcknowledged()
	require.Equal(t, e.replicationV2.end, offset)
	require.Zero(t, behind, "a replica at the end has nothing outstanding")
}

// An offset only means something inside the epoch that produced it. A pull from
// another history names a position in a stream this primary never wrote.
func TestAcknowledgementIgnoresOffsetsFromAnotherEpoch(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	setupReplicationV2On(t, e)
	t.Cleanup(e.resetReplicaAcknowledgement)
	runOn(t, e, "SET", "k", "v")

	pullV2On(t, e, e.replication.epoch, e.replicationV2.end, "", 0)
	trusted, _, _ := e.ReplicationAcknowledged()
	require.Equal(t, e.replicationV2.end, trusted)

	// A stale or foreign epoch must not move it, in either direction.
	runOn(t, e, "KEEL.REPL.PULL2", "0123456789abcdef0123456789abcdef", "999999", "", "0",
		strconv.FormatUint(e.CurrentTerm(), 10))
	after, _, _ := e.ReplicationAcknowledged()
	require.Equal(t, trusted, after, "a foreign epoch's offset must not be believed")
}

// The recorded value is the furthest any replica has reached, not the nearest,
// so it cannot answer "do n replicas hold this write". This pins that down so
// nobody later reads it as a quorum signal.
func TestAcknowledgementTracksTheFurthestReplicaNotTheNearest(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	setupReplicationV2On(t, e)
	t.Cleanup(e.resetReplicaAcknowledgement)
	runOn(t, e, "SET", "k", "v")
	end := e.replicationV2.end

	pullV2On(t, e, e.replication.epoch, end, "", 0) // a replica that is caught up
	pullV2On(t, e, e.replication.epoch, 0, "", 0)   // and one that is far behind

	offset, behind, _ := e.ReplicationAcknowledged()
	require.Equal(t, end, offset,
		"the furthest is what is recorded, which is why this is lag and not durability")
	require.Zero(t, behind)
}

func TestInfoReportsReplicationLag(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	setupReplicationV2On(t, e)
	t.Cleanup(e.resetReplicaAcknowledgement)
	runOn(t, e, "SET", "k", "v")
	pullV2On(t, e, e.replication.epoch, 0, "", 0)

	info, ok := runOn(t, e, "INFO", "replication").(string)
	require.True(t, ok)
	require.Contains(t, info, "replication_acked_offset:0")
	require.Contains(t, info, "replication_lag_bytes:"+strconv.FormatUint(e.replicationV2.end, 10))
	require.NotContains(t, info, "replication_acked_age_ms:-1", "an acknowledgement arrived")
}

func TestReplicaProgressIgnoresFutureCursor(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	setupReplicationV2On(t, e)
	runOn(t, e, "SET", "k", "v")
	end := e.replicationV2.end
	pullV2On(t, e, e.replication.epoch, end, "", 0)
	require.Equal(t, end, e.replicaAck.offset)
	observed := time.Unix(100, 0)
	e.replicaAck.at = observed
	_ = e.cmdReplicationPullV2([]string{e.replication.epoch, strconv.FormatUint(end+1, 10), "", "0", strconv.FormatUint(e.CurrentTerm(), 10)})
	require.Equal(t, end, e.replicaAck.offset, "an offset beyond this stream is not progress")
	require.Equal(t, observed, e.replicaAck.at)
}

func TestReplicaProgressLowerCursorDoesNotRefreshBestAge(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	setupReplicationV2On(t, e)
	runOn(t, e, "SET", "k", "v")
	pullV2On(t, e, e.replication.epoch, e.replicationV2.end, "", 0)
	require.Equal(t, e.replicationV2.end, e.replicaAck.offset)
	observed := time.Unix(100, 0)
	e.replicaAck.at = observed
	pullV2On(t, e, e.replication.epoch, 0, "", 0)
	require.Equal(t, e.replicationV2.end, e.replicaAck.offset)
	require.Equal(t, observed, e.replicaAck.at, "a lagging peer must not make the best cursor look fresh")
}

func TestReplicaProgressIgnoresMalformedAndSnapshotPulls(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	for _, cursor := range []struct{ name, snapshot, part string }{
		{"malformed delta", "", "1"},
		{"snapshot transfer", "stale-snapshot", "0"},
	} {
		t.Run(cursor.name, func(t *testing.T) {
			setupReplicationV2On(t, e)
			runOn(t, e, "SET", "k", "v")
			pullV2On(t, e, e.replication.epoch, e.replicationV2.end, "", 0)
			require.Equal(t, e.replicationV2.end, e.replicaAck.offset)
			observed := time.Unix(100, 0)
			e.replicaAck.at = observed
			_ = e.cmdReplicationPullV2([]string{e.replication.epoch, strconv.FormatUint(e.replicationV2.end, 10), cursor.snapshot, cursor.part, strconv.FormatUint(e.CurrentTerm(), 10)})
			require.Equal(t, e.replicationV2.end, e.replicaAck.offset)
			require.Equal(t, observed, e.replicaAck.at, "only a validated delta cursor confirms stream progress")
		})
	}
}

func TestReplicaProgressResetsWhenPrimaryChangesEpoch(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	setupReplicationV2On(t, e)
	runOn(t, e, "SET", "k", "v")
	require.NotZero(t, e.replicationV2.end)
	pullV2On(t, e, e.replication.epoch, e.replicationV2.end, "", 0)
	require.Equal(t, e.replicationV2.end, e.replicaAck.offset)
	previousEpoch := e.replication.epoch
	e.invalidateReplicationV2()
	require.NotEqual(t, previousEpoch, e.replication.epoch)
	offset, behind, age := e.ReplicationAcknowledged()
	require.Zero(t, offset)
	require.Zero(t, behind)
	require.EqualValues(t, -1, age, "old epoch progress is not evidence for the new stream")
	pullV2On(t, e, e.replication.epoch, 0, "", 0)
	offset, behind, age = e.ReplicationAcknowledged()
	require.Zero(t, offset)
	require.Zero(t, behind)
	require.GreaterOrEqual(t, age, int64(0), "the new stream can record a lower cursor")
}
