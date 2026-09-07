package server

import (
	"bytes"
	"github.com/brandopakel/keel/internal/core"
	"github.com/brandopakel/keel/internal/core/io_multiplexing"
	"github.com/stretchr/testify/require"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

func TestExecuteRunYieldsDeepPipelines(t *testing.T) {
	c := &client{fd: -1}
	for i := 0; i < 257; i++ {
		c.cmds = append(c.cmds, &core.Command{Cmd: "PING"})
	}
	var arena replyArena
	require.True(t, executeRun(c, &arena))
	require.GreaterOrEqual(t, len(c.cmds), 193, "at most 64 commands per turn")
	require.Less(t, len(c.cmds), 257, "each turn must make progress")
	if c.inArena {
		c.out = arena.buf[c.outStart:c.outEnd]
	}
	require.Equal(t, 257-len(c.cmds), bytes.Count(c.out, []byte("+PONG\r\n")))
	t.Logf("client struct bytes=%d", unsafe.Sizeof(client{}))
}

func TestReadCommandsBoundsDecodedPipelines(t *testing.T) {
	r, w := socketPair(t)
	require.NoError(t, syscall.SetNonblock(r, true))
	require.NoError(t, syscall.SetNonblock(w, true))
	payload := bytes.Repeat([]byte("*1\r\n$4\r\nPING\r\n"), 129)
	n, err := syscall.Write(w, payload)
	require.NoError(t, err)
	require.Equal(t, len(payload), n)
	c := &client{fd: r}
	scratch := make([]byte, readChunkSize)
	got, err := c.readCommands(scratch)
	require.NoError(t, err)
	require.LessOrEqual(t, len(got), 64)
	count := len(got)
	for count < 129 {
		got, err = c.readCommands(scratch)
		require.NoError(t, err)
		require.NotEmpty(t, got, "buffered complete commands must progress without another socket read")
		require.LessOrEqual(t, len(got), 64)
		count += len(got)
	}
	require.Equal(t, 129, count)
	require.Nil(t, c.buf)
}

func TestLargeFirstReplyYieldsWithoutArenaCopy(t *testing.T) {
	core.ResetStores()
	t.Cleanup(core.ResetStores)
	var sink replyBuffer
	responseRw(&core.Command{Cmd: "SET", Args: []string{"large", strings.Repeat("x", 1<<20)}}, &sink)
	c := &client{fd: -1, cmds: []*core.Command{{Cmd: "GET", Args: []string{"large"}}, {Cmd: "PING"}}}
	var arena replyArena
	require.True(t, executeRun(c, &arena))
	require.Len(t, c.cmds, 1)
	require.False(t, c.inArena)
	require.Empty(t, arena.buf, "do not copy a large first reply into the shared arena")
	require.Greater(t, len(c.out), 1<<20)
}

func TestQueuedReadsRejectClosedReusedAndBlockedConnections(t *testing.T) {
	oldClients, oldQueue := clients, queuedClientReads
	clients, queuedClientReads = make(map[int]*client), nil
	t.Cleanup(func() { clients, queuedClientReads = oldClients, oldQueue })
	for fd := 1; fd <= 5; fd++ {
		c := &client{fd: fd, bufferedReady: true}
		clients[fd] = c
		queueClientRead(c)
		queueClientRead(c)
	}
	require.Len(t, queuedClientReads, 5, "one queue entry per connection")
	clients[2] = &client{fd: 2}
	delete(clients, 3)
	clients[4].appendHeld = true
	clients[5].out = []byte("pending")
	ready := takeQueuedReads(nil)
	require.Equal(t, []*client{clients[1]}, ready)
	require.Empty(t, queuedClientReads)
	for _, c := range clients {
		require.False(t, c.readQueued)
	}
	clients[4].appendHeld = false
	clients[5].out = nil
	queueClientRead(clients[4])
	queueClientRead(clients[5])
	require.Equal(t, []*client{clients[4], clients[5]}, takeQueuedReads(nil))
}

func TestPipelineContinuationWaitsForAppendAndReplyDrain(t *testing.T) {
	oldClients, oldQueue := clients, queuedClientReads
	oldTotal, oldInput, oldReply := retainedClientBytes, retainedInputBytes, retainedReplyBytes
	clients, queuedClientReads = make(map[int]*client), nil
	t.Cleanup(func() {
		clients, queuedClientReads = oldClients, oldQueue
		retainedClientBytes, retainedInputBytes, retainedReplyBytes = oldTotal, oldInput, oldReply
	})
	r, w := socketPair(t)
	require.NoError(t, syscall.SetNonblock(w, true))
	c := &client{fd: r, out: []byte("+OK\r\n"), bufferedReady: true, appendOffset: core.AppendReadyOffset() + 1}
	clients[r] = c
	mux := &recordingMonitor{operations: make(map[int]io_multiplexing.Operation)}
	var q orderedAppend
	var arena replyArena
	require.Empty(t, q.gate([]*client{c}, &arena, mux))
	require.True(t, c.appendHeld)
	savedReply := c.out
	queueClientRead(c)
	require.True(t, c.readQueued)
	c.out = nil // Isolate appendHeld from the independent reply-drain guard.
	require.Empty(t, takeQueuedReads(nil), "no continuation before the persistence offset is ready")
	var buf [16]byte
	_, err := syscall.Read(w, buf[:])
	require.ErrorIs(t, err, syscall.EAGAIN)
	// Simulate completion of the offset: only its subsequent reply drain queues
	// the continuation. The real worker's offset transition has separate tests.
	c.appendHeld = false
	c.out = savedReply
	pool := newIOPool(1)
	t.Cleanup(pool.stop)
	flushClientReplies(pool, mux, []*client{c})
	n, err := syscall.Read(w, buf[:])
	require.NoError(t, err)
	require.Equal(t, "+OK\r\n", string(buf[:n]))
	require.Equal(t, []*client{c}, takeQueuedReads(nil))
}

func TestPipelineContinuationCoalescesWakeups(t *testing.T) {
	oldClients, oldQueue, oldWake := clients, queuedClientReads, wakeFn
	clients, queuedClientReads = make(map[int]*client), nil
	wakes := 0
	setWaker(func() { wakes++ })
	t.Cleanup(func() {
		clients, queuedClientReads = oldClients, oldQueue
		setWaker(oldWake)
	})
	for fd := 1; fd <= 512; fd++ {
		c := &client{fd: fd, bufferedReady: true}
		clients[fd] = c
		queueClientRead(c)
		queueClientRead(c)
	}
	require.Equal(t, 1, wakes)
	require.Len(t, takeQueuedReads(nil), 512)
	queueClientRead(clients[1])
	require.Equal(t, 2, wakes, "a newly nonempty queue needs a fresh wakeup")
}
