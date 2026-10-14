package server

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestClientMemoryIsReportedInRedisTerms: connected_clients leaves the
// replicas out, mem_clients_normal and mem_clients_slaves split what the
// connections retain between the others and the replicas, and the recent
// maxima are the most one connection held, a replica's too, in the last seven
// seconds, as Redis's eight slots keep them.
func TestClientMemoryIsReportedInRedisTerms(t *testing.T) {
	saved, savedPeaks := clients, clientPeaks
	t.Cleanup(func() { clients, clientPeaks = saved, savedPeaks })
	clients, clientPeaks = map[int]*client{}, clientBufferPeaks{}
	clients[1] = &client{accountedInput: 100, accountedReply: 2000}
	clients[2] = &client{accountedInput: 5000, accountedReply: 10}
	clients[3] = &client{accountedInput: 7, accountedReply: 9000, replica: true}

	assert.Equal(t, clientMemory{connected: 2, normal: 7110, replicas: 9007, recentInput: 5000, recentReply: 9000},
		sampleClientMemory(1000))

	delete(clients, 2)
	delete(clients, 3)
	m := sampleClientMemory(1006)
	assert.Equal(t, 5000, m.recentInput, "a connection gone counts while its second is in the window")
	assert.Equal(t, 9000, m.recentReply)
	m = sampleClientMemory(1007)
	assert.Equal(t, 100, m.recentInput, "seven seconds on, its second has left the window")
	assert.Equal(t, 2000, m.recentReply)
	assert.Equal(t, clientMemory{connected: 1, normal: 2100, recentInput: 100, recentReply: 2000}, m)
}

// TestClientBufferPeaksReuseTheSlotOfEightSecondsAgo: a slot holds one second;
// the same slot eight seconds on starts again from nothing.
func TestClientBufferPeaksReuseTheSlotOfEightSecondsAgo(t *testing.T) {
	var p clientBufferPeaks
	p.note(16, 300, 40)
	p.note(16, 200, 50)
	in, out := p.recent(16)
	assert.Equal(t, [2]int{300, 50}, [2]int{in, out}, "the most of each within the second")
	p.note(24, 1, 2)
	in, out = p.recent(24)
	assert.Equal(t, [2]int{1, 2}, [2]int{in, out})
}
