package server

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNetworkBytesAreCountedAsRedisCountsThem: a client's request is input
// and its reply output, each read and write counted; a connection that has
// pulled the log as a replica is sent replication output; a replica's
// connection to its primary reads replication input.
func TestNetworkBytesAreCountedAsRedisCountsThem(t *testing.T) {
	resetNetStats()
	t.Cleanup(resetNetStats)
	request := []byte("*1\r\n$4\r\nPING\r\n")
	cs := pairedClients(t, 1, request)
	c := cs[0]
	_, err := c.readCommands(testScratch[:])
	require.NoError(t, err)
	assert.Equal(t, uint64(len(request)), netInputBytes.Load())
	assert.Equal(t, uint64(1), readsProcessed.Load())

	pool := &ioPool{threads: 1}
	c.out = []byte("+PONG\r\n")
	pool.serve(c, true, nil)
	assert.Equal(t, uint64(7), netOutputBytes.Load())
	assert.Equal(t, uint64(1), writesProcessed.Load())

	assert.True(t, isReplicationPull("KEEL.REPL.PULL2"))
	c.replica = true
	c.out = []byte("$3\r\nlog\r\n")
	pool.serve(c, true, nil)
	assert.Equal(t, uint64(7), netOutputBytes.Load(), "a replica's is replication output")
	assert.Equal(t, uint64(9), netReplOutputBytes.Load())

	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	go func() { b.Write([]byte("+OK\r\n")) }()
	buf := make([]byte, 16)
	n, err := replicaConn{a}.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, uint64(n), netReplInputBytes.Load(), "what a replica reads from its primary")
	go func() { b.Read(buf) }()
	_, err = replicaConn{a}.Write([]byte("PING"))
	require.NoError(t, err)
	assert.Equal(t, uint64(7+4), netOutputBytes.Load(), "what it writes, ordinary output")

	resetNetStats()
	assert.Zero(t, netInputBytes.Load()+netOutputBytes.Load()+netReplInputBytes.Load()+netReplOutputBytes.Load()+
		readsProcessed.Load()+writesProcessed.Load())
}
