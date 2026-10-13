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
			ClosedUnread: 8, RunsUnreplied: 9, ConnectionsReceived: 11, ConnectionsRejected: 12}
	})
	assert.Contains(t, string(e.cmdINFO([]string{"clients"})), "# Clients\r\nconnected_clients:3\r\ncluster_connections:0\r\n"+
		"blocked_clients:0\r\ntracking_clients:0\r\npubsub_clients:0\r\nwatching_clients:0\r\n"+
		"total_watched_keys:0\r\ntotal_blocking_keys:0\r\ntotal_blocking_keys_on_nokey:0\r\n"+
		"retained_input_bytes:10\r\nretained_reply_bytes:20\r\nretained_client_bytes:30\r\n"+
		"request_allocation_peak_bytes:40\r\nrequest_allocation_refusals:5\r\n"+
		"clients_closed_slow:6\r\nclients_closed_unanswered:7\r\nclients_closed_unread:8\r\nclients_closed_unreplied:9\r\n\r\n")
	assert.Contains(t, string(e.cmdINFO([]string{"stats"})), "# Stats\r\ntotal_connections_received:11\r\n"+
		"total_commands_processed:0\r\ninstantaneous_ops_per_sec:0\r\nrejected_connections:12\r\nexpired_keys:0\r\nevicted_keys:0\r\n")
	assert.NotContains(t, string(e.cmdINFO([]string{"memory"})), "connected_clients")
	assert.NotContains(t, string(other.cmdINFO([]string{"clients"})), "connected_clients", "the hook is e's alone")
	assert.NotContains(t, string(other.cmdINFO([]string{"stats"})), "total_connections_received")
	e.SetClientBuffers(nil)
	assert.NotContains(t, string(e.cmdINFO(nil)), "connected_clients")
}
