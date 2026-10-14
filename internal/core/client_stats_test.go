package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestINFOClientBuffersHasExplicitScopeAndStableValues: INFO reports the
// connections of the transport driving its engine, through the hook that
// transport installed on that engine, in the fields and order it always has;
// an engine with no hook, another engine among them, reports none.
func TestINFOClientBuffersHasExplicitScopeAndStableValues(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	other := newTestEngine(t, Options{})
	require.NotContains(t, string(e.cmdINFO([]string{"clients"})), "connected_clients")
	e.SetClientBuffers(func() ClientBufferStats {
		return ClientBufferStats{Connected: 3, InputBytes: 10, ReplyBytes: 20, TotalBytes: 30,
			RequestAllocationPeak: 40, RequestAllocationRefusals: 5, ClosedSlow: 6, ClosedUnanswered: 7,
			ClosedUnread: 8, RunsUnreplied: 9, ConnectionsReceived: 11, ConnectionsRejected: 12,
			RecentMaxInput: 13, RecentMaxReply: 14, NormalBytes: 15, ReplicaBytes: 16}
	})
	assert.Contains(t, string(e.cmdINFO([]string{"clients"})), "# Clients\r\nconnected_clients:3\r\ncluster_connections:0\r\n"+
		"client_recent_max_input_buffer:13\r\nclient_recent_max_output_buffer:14\r\n"+
		"blocked_clients:0\r\ntracking_clients:0\r\npubsub_clients:0\r\nwatching_clients:0\r\n"+
		"total_watched_keys:0\r\ntotal_blocking_keys:0\r\ntotal_blocking_keys_on_nokey:0\r\n"+
		"retained_input_bytes:10\r\nretained_reply_bytes:20\r\nretained_client_bytes:30\r\n"+
		"request_allocation_peak_bytes:40\r\nrequest_allocation_refusals:5\r\n"+
		"clients_closed_slow:6\r\nclients_closed_unanswered:7\r\nclients_closed_unread:8\r\nclients_closed_unreplied:9\r\n\r\n")
	assert.Contains(t, string(e.cmdINFO([]string{"stats"})), "# Stats\r\ntotal_connections_received:11\r\n"+
		"total_commands_processed:0\r\ninstantaneous_ops_per_sec:0\r\n"+
		"total_net_input_bytes:0\r\ntotal_net_output_bytes:0\r\ntotal_net_repl_input_bytes:0\r\ntotal_net_repl_output_bytes:0\r\n"+
		"instantaneous_input_kbps:0.00\r\ninstantaneous_output_kbps:0.00\r\ninstantaneous_input_repl_kbps:0.00\r\ninstantaneous_output_repl_kbps:0.00\r\n"+
		"rejected_connections:12\r\nsync_full:0\r\nexpired_subkeys:0\r\nexpired_subkeys_active:0\r\nexpired_keys:0\r\n"+
		"expired_keys_active:0\r\nexpired_stale_perc:0.00\r\nexpired_time_cap_reached_count:0\r\nexpire_cycle_cpu_milliseconds:0\r\n"+
		"evicted_keys:0\r\n")
	assert.NotContains(t, string(e.cmdINFO([]string{"memory"})), "connected_clients")
	assert.Contains(t, string(e.cmdINFO([]string{"memory"})), "maxmemory_policy:allkeys-lru\r\n"+
		"mem_clients_slaves:16\r\nmem_clients_normal:15\r\nmem_clients_normal_shared:0\r\nmem_clients_normal_unshared:0\r\n"+
		"active_defrag_running:0\r\n")
	assert.NotContains(t, string(other.cmdINFO([]string{"memory"})), "mem_clients", "an engine with no transport has no clients")
	assert.NotContains(t, string(other.cmdINFO([]string{"clients"})), "connected_clients", "the hook is e's alone")
	assert.NotContains(t, string(other.cmdINFO([]string{"stats"})), "total_connections_received")
	e.SetClientBuffers(nil)
	assert.NotContains(t, string(e.cmdINFO(nil)), "connected_clients")
}
