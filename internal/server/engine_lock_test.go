package server

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/brandopakel/keel/internal/core"
)

// TestTheLoopRunsOnlyWhileItHoldsItsEnginesLock: the event loop holds its
// engine's lock for every cycle and lets go only while it waits for something
// to be ready, so a goroutine that takes the lock keeps the loop from running
// anything on the engine, and may run commands on it itself meanwhile. Once it
// lets go, the loop answers what arrived in between, from the keyspace the
// lock's holder left. Under -race, the loop touching the engine while the test
// holds the lock would be reported as a race with the test's own command.
func TestTheLoopRunsOnlyWhileItHoldsItsEnginesLock(t *testing.T) {
	withFreshShutdownState(t)
	e := newTestEngine(t, core.Options{})

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := probe.Addr().(*net.TCPAddr).Port
	require.NoError(t, probe.Close())

	var wg sync.WaitGroup
	wg.Add(1)
	served := make(chan error, 1)
	go func() { served <- RunAsyncTCPServer(&wg, e, Options{Host: "127.0.0.1", Port: port}) }()
	t.Cleanup(func() {
		requestShutdown()
		wake()
		wg.Wait()
		for fd := range clients {
			delete(clients, fd)
		}
		require.NoError(t, <-served)
	})

	// The loop is up once it answers: a connection alone could reach the
	// probe's port before the loop has bound it.
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	var conn net.Conn
	var r *bufio.Reader
	deadline := time.Now().Add(5 * time.Second)
	for conn == nil {
		require.True(t, time.Now().Before(deadline), "the loop never answered on %s", addr)
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		reader := bufio.NewReader(c)
		c.SetDeadline(time.Now().Add(time.Second))
		if _, err := c.Write(encodeCmd("PING")); err == nil {
			if line, err := reader.ReadString('\n'); err == nil && line == "+PONG\r\n" {
				conn, r = c, reader
				continue
			}
		}
		c.Close()
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(func() { conn.Close() })

	// Taken once the loop is parked. From here the test is the engine's only
	// user: it writes a key the loop has not seen.
	e.Lock()
	var reply bytes.Buffer
	require.NoError(t, e.EvalAndResponse(&core.Command{Cmd: "SET", Args: []string{"k", "set-while-held"}}, &reply))
	require.Equal(t, "+OK\r\n", reply.String())

	// The request wakes the loop, which must not run it while the lock is
	// held. A slow machine can only delay a reply, never make one arrive, so
	// waiting a while for none is not a timing assertion.
	_, err = conn.Write(encodeCmd("GET", "k"))
	require.NoError(t, err)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(300*time.Millisecond)))
	_, err = r.ReadByte()
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		e.Unlock()
		t.Fatalf("the loop answered, or failed, while another goroutine held its engine's lock: %v", err)
	}

	// Let go, and the loop answers, from the keyspace the test left.
	e.Unlock()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	header, err := r.ReadString('\n')
	require.NoError(t, err)
	body, err := r.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "$14\r\nset-while-held\r\n", header+body)
}
