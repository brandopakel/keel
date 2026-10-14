package server

import (
	"bytes"
	"testing"
	"time"

	"github.com/brandopakel/keel/internal/core"
	"github.com/brandopakel/keel/internal/resp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReplicaLinkCountsAsRedisDoes: every connection attempted counts, the
// current count starting again once an established link is lost; the link is
// up from its first successful pull, down from its connection's end, and the
// outages that end add to the time disconnected.
func TestReplicaLinkCountsAsRedisDoes(t *testing.T) {
	var l replicaLink
	start := time.Unix(1000, 0)
	l.dialing()
	l.lost(start) // the dial failed: never up, so nothing changes but the counts
	l.dialing()
	l.pulled(start.Add(time.Second))
	s := l.state()
	assert.True(t, s.Up)
	assert.Equal(t, [2]int64{2, 2}, [2]int64{s.Attempts, s.TotalAttempts})
	assert.Equal(t, start.Add(time.Second), s.UpSince)
	assert.True(t, s.DownSince.IsZero(), "never down since it was first up")
	assert.Zero(t, s.Disconnected, "not counted before the link was first up")

	l.pulled(start.Add(3 * time.Second))
	assert.Equal(t, start.Add(3*time.Second), l.state().LastIO)
	assert.Equal(t, start.Add(time.Second), l.state().UpSince, "still the same link")

	l.lost(start.Add(4 * time.Second))
	s = l.state()
	assert.False(t, s.Up)
	assert.Equal(t, start.Add(4*time.Second), s.DownSince)
	assert.Equal(t, [2]int64{0, 2}, [2]int64{s.Attempts, s.TotalAttempts}, "a lost link starts the current count again")

	l.dialing()
	l.pulled(start.Add(10 * time.Second))
	s = l.state()
	assert.True(t, s.Up)
	assert.Equal(t, [2]int64{1, 3}, [2]int64{s.Attempts, s.TotalAttempts})
	assert.Equal(t, 6*time.Second, s.Disconnected)
}

// TestConnectedReplicasAreTheAcceptedPulls: a connection whose pull the
// engine accepted is a connected replica, with where its last pull started
// and when; they are listed in the order they connected.
func TestConnectedReplicasAreTheAcceptedPulls(t *testing.T) {
	saved := clients
	clients = map[int]*client{}
	t.Cleanup(func() { clients = saved })
	e := newTestEngine(t, core.Options{ReplicationFeed: true})
	first := &client{engine: e, id: 7, addr: "10.0.0.2:51234", authenticated: true}
	second := &client{engine: e, id: 3, addr: "[::1]:51235", authenticated: true}
	plain := &client{engine: e, id: 1, addr: "10.0.0.3:40000", authenticated: true}
	clients[10], clients[11], clients[12] = first, second, plain
	var out bytes.Buffer
	before := time.Now()
	first.respond(resp.ParsedCommand("KEEL.REPL.PULL", []string{"epoch", "0"}), &out)
	second.respond(resp.ParsedCommand("KEEL.REPL.PULL", []string{"epoch", "0"}), &out)
	second.respond(resp.ParsedCommand("KEEL.REPL.PULL", []string{"epoch", "x"}), &out) // refused: keeps its last
	plain.respond(resp.ParsedCommand("PING", nil), &out)

	replicas := connectedReplicas()
	require.Len(t, replicas, 2)
	assert.Equal(t, core.ConnectedReplica{IP: "::1", Port: 51235, Offset: 0, Pulled: replicas[0].Pulled}, replicas[0],
		"connection 3 connected before connection 7")
	assert.Equal(t, "10.0.0.2", replicas[1].IP)
	assert.Equal(t, 51234, replicas[1].Port)
	assert.False(t, replicas[1].Pulled.Before(before))
}
