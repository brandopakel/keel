package core

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// feedingPrimary is an engine that feeds replicas over protocol, with its log
// open and its stream's epoch begun, as the server makes one.
func feedingPrimary(t *testing.T, protocol int) *Engine {
	t.Helper()
	e := newTestEngine(t, Options{ReplicationFeed: true, ReplicationProtocol: protocol})
	require.NoError(t, e.OpenAOF(filepath.Join(t.TempDir(), "primary.aof")))
	require.NoError(t, e.InitReplication())
	return e
}

// replicationInfoOf is INFO replication's text on e.
func replicationInfoOf(t *testing.T, e *Engine) string {
	t.Helper()
	text, ok := runOn(t, e, "INFO", "replication").(string)
	require.True(t, ok)
	return text
}

// TestReplicationInfoIsRedisFirst: a primary's section starts with Redis's
// fields in Redis's order, its stream's identity and offset among them, and
// Keel's own lines follow; the replicas its transport holds are its
// connected_slaves and slave<n> lines.
func TestReplicationInfoIsRedisFirst(t *testing.T) {
	t.Parallel()
	e := feedingPrimary(t, 1)
	runOn(t, e, "SET", "k", "v")
	// Protocol 1 seals what changed into a batch when a replica pulls.
	runOn(t, e, "KEEL.REPL.PULL", "", "0")
	info := replicationInfoOf(t, e)
	epoch := infoFields(t, info)["primary_epoch"]
	require.Len(t, epoch, 32)
	assert.True(t, strings.HasPrefix(info, "# Replication\r\nrole:master\r\nconnected_slaves:0\r\n"+
		"master_failover_state:no-failover\r\nmaster_replid:"+epoch+"\r\nmaster_replid2:"+noReplicationID+"\r\n"+
		"master_repl_offset:1\r\nsecond_repl_offset:-1\r\n"+
		"repl_backlog_active:1\r\nrepl_backlog_size:16777216\r\nrepl_backlog_first_byte_offset:1\r\n"), info)
	fields := infoFields(t, info)
	assert.Equal(t, fields["replication_history_bytes"], fields["repl_backlog_histlen"])
	assert.Equal(t, "1", fields["primary_offset"], "Keel's own lines stay")
	assert.NotContains(t, info, "role:primary")

	pulled := time.Now().Add(-3 * time.Second)
	e.SetReplicationTransport(func() ReplicationTransport {
		return ReplicationTransport{Replicas: []ConnectedReplica{
			{IP: "10.0.0.2", Port: 51234, Offset: 1, Pulled: pulled},
			{IP: "::1", Port: 51235, Offset: 0, Pulled: time.Now()}}}
	})
	info = replicationInfoOf(t, e)
	assert.Contains(t, info, "connected_slaves:2\r\n"+
		"slave0:ip=10.0.0.2,port=51234,state=online,offset=1,lag=3,io-thread=0\r\n"+
		"slave1:ip=::1,port=51235,state=online,offset=0,lag=0,io-thread=0\r\nmaster_failover_state:")

	plain := newTestEngine(t, Options{})
	assert.Contains(t, replicationInfoOf(t, plain), "repl_backlog_active:0\r\n", "no replicas fed, no history kept")
}

// TestReplicaInfoReportsItsLinkAsRedisDoes: a replica's section has Redis's
// master_* fields from its transport's link, up or down, and its stream's
// identity and offset are its primary's as it follows them.
func TestReplicaInfoReportsItsLinkAsRedisDoes(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{ReplicaOf: "10.0.0.1:6380"})
	info := replicationInfoOf(t, e)
	assert.True(t, strings.HasPrefix(info, "# Replication\r\nrole:slave\r\nmaster_host:10.0.0.1\r\nmaster_port:6380\r\n"+
		"master_link_status:down\r\nmaster_last_io_seconds_ago:-1\r\nmaster_sync_in_progress:0\r\n"+
		"slave_read_repl_offset:0\r\nslave_repl_offset:0\r\nreplica_full_sync_buffer_size:0\r\nreplica_full_sync_buffer_peak:0\r\n"+
		"master_current_sync_attempts:0\r\nmaster_total_sync_attempts:0\r\n"+
		"master_link_down_since_seconds:-1\r\ntotal_disconnect_time_sec:0\r\nslave_read_only:1\r\nconnected_slaves:0\r\n"), info)

	now := time.Now()
	link := ReplicationLink{Up: true, UpSince: now.Add(-20 * time.Second), LastIO: now.Add(-2 * time.Second),
		Attempts: 1, TotalAttempts: 3, Disconnected: 7 * time.Second}
	e.SetReplicationTransport(func() ReplicationTransport { return ReplicationTransport{Link: link} })
	fields := infoFields(t, replicationInfoOf(t, e))
	for name, want := range map[string]string{"master_link_status": "up", "master_last_io_seconds_ago": "2",
		"master_sync_in_progress": "1", "master_current_sync_attempts": "1", "master_total_sync_attempts": "3",
		"master_link_up_since_seconds": "20", "master_client_io_thread": "0", "total_disconnect_time_sec": "7"} {
		assert.Equal(t, want, fields[name], name)
	}
	assert.NotContains(t, fields, "master_link_down_since_seconds", "up, so no down-since")

	link = ReplicationLink{DownSince: now.Add(-5 * time.Second), Attempts: 2, TotalAttempts: 4, Disconnected: 7 * time.Second}
	fields = infoFields(t, replicationInfoOf(t, e))
	assert.Equal(t, "down", fields["master_link_status"])
	assert.Equal(t, "-1", fields["master_last_io_seconds_ago"])
	assert.Equal(t, "0", fields["master_sync_in_progress"], "not syncing while the link is down")
	assert.Equal(t, "5", fields["master_link_down_since_seconds"])
	assert.Equal(t, "12", fields["total_disconnect_time_sec"], "the current outage counts too")
}

// TestFullSyncsAreCounted: sync_full counts the full syncs a primary serves,
// and an incremental pull is none; CONFIG RESETSTAT resets it.
func TestFullSyncsAreCounted(t *testing.T) {
	t.Parallel()
	e := feedingPrimary(t, 1)
	runOn(t, e, "SET", "k", "v")
	syncFull := func() string { return statsOn(t, e)["sync_full"] }
	assert.Equal(t, "0", syncFull())
	runOn(t, e, "KEEL.REPL.PULL", "", "0")
	assert.Equal(t, "1", syncFull())
	epoch := infoFields(t, replicationInfoOf(t, e))["primary_epoch"]
	runOn(t, e, "KEEL.REPL.PULL", epoch, "1")
	assert.Equal(t, "1", syncFull(), "an incremental pull is no full sync")
	runOn(t, e, "CONFIG", "RESETSTAT")
	assert.Equal(t, "0", syncFull())
}
