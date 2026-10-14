package server

import (
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestPeerAddrIsRedisFormat: ip:port, [ip]:port for IPv6, path:0 for a Unix
// socket, as Redis's formatAddr writes a peer.
func TestPeerAddrIsRedisFormat(t *testing.T) {
	assert.Equal(t, "127.0.0.1:6379", peerAddr(&syscall.SockaddrInet4{Port: 6379, Addr: [4]byte{127, 0, 0, 1}}))
	assert.Equal(t, "[::1]:50000", peerAddr(&syscall.SockaddrInet6{Port: 50000, Addr: [16]byte{15: 1}}))
	assert.Equal(t, "/tmp/keel.sock:0", peerAddr(&syscall.SockaddrUnix{Name: "/tmp/keel.sock"}))
	c := &client{addr: "10.0.0.2:4242", name: "worker"}
	assert.Equal(t, "10.0.0.2:4242", c.PeerAddr())
	assert.Equal(t, "worker", c.ClientName())
}
