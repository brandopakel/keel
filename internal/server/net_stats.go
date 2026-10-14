package server

import (
	"io"
	"net"
	"sync/atomic"
)

// The bytes and the reads and writes INFO stats reports, as Redis counts
// them (networking.c): what clients send and are sent, apart from what goes
// between a primary and its replicas, which Redis counts as replication
// traffic; and every read and write the I/O path makes, whichever thread
// makes it. Atomic, as Redis's are, because the I/O threads count too.
var (
	netInputBytes, netOutputBytes         atomic.Uint64
	netReplInputBytes, netReplOutputBytes atomic.Uint64
	readsProcessed, writesProcessed       atomic.Uint64
)

// resetNetStats is CONFIG RESETSTAT's for them, as Redis's resetServerStats
// zeroes its own.
func resetNetStats() {
	for _, n := range []*atomic.Uint64{&netInputBytes, &netOutputBytes, &netReplInputBytes, &netReplOutputBytes,
		&readsProcessed, &writesProcessed} {
		n.Store(0)
	}
}

// isReplicationPull is whether cmd makes the connection that sent it a
// replica's: what it is sent from then on is replication traffic, as what
// Redis sends a client it has made a replica (CLIENT_SLAVE) is.
func isReplicationPull(name string) bool {
	return name == "KEEL.REPL.PULL" || name == "KEEL.REPL.PULL2"
}

// firstByte passes a reply through and keeps its first byte, which is '-'
// when the reply is an error.
type firstByte struct {
	io.ReadWriter
	b byte
}

func (f *firstByte) Write(p []byte) (int, error) {
	if f.b == 0 && len(p) > 0 {
		f.b = p[0]
	}
	return f.ReadWriter.Write(p)
}

// replicaConn is a replica's connection to its primary, counted as Redis
// counts its master's: what it reads is replication input, what it writes,
// its own requests, ordinary output.
type replicaConn struct{ net.Conn }

func (c replicaConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	readsProcessed.Add(1)
	netReplInputBytes.Add(uint64(max(n, 0)))
	return n, err
}

func (c replicaConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	writesProcessed.Add(1)
	netOutputBytes.Add(uint64(max(n, 0)))
	return n, err
}
