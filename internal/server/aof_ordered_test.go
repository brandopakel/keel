package server

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/brandopakel/keel/internal/config"
	"github.com/brandopakel/keel/internal/core"
	"github.com/brandopakel/keel/internal/core/io_multiplexing"
	"github.com/stretchr/testify/require"
)

type recordingMonitor struct {
	io_multiplexing.IOMultiplexer
	operations map[int]io_multiplexing.Operation
}

func (m *recordingMonitor) Monitor(e io_multiplexing.Event) error {
	m.operations[e.Fd] = e.Op
	return nil
}

func TestOrderedRepliesAndDependentReadWaitForPersistence(t *testing.T) {
	core.ResetStores()
	oldClients := clients
	clients = make(map[int]*client)
	t.Cleanup(func() { clients = oldClients })
	oldPolicy, oldAsync, oldConcurrent := config.AOFFsync, config.AOFAsyncAppend, config.AOFConcurrentAppend
	oldRetained := retainedClientBytes
	config.AOFFsync, config.AOFAsyncAppend, config.AOFConcurrentAppend = config.FsyncAlways, true, true
	defer func() {
		core.CloseAOF()
		config.AOFFsync, config.AOFAsyncAppend, config.AOFConcurrentAppend = oldPolicy, oldAsync, oldConcurrent
		retainedClientBytes = oldRetained
	}()
	require.NoError(t, core.OpenAOF(filepath.Join(t.TempDir(), "log")))
	mux := &recordingMonitor{operations: make(map[int]io_multiplexing.Operation)}
	var q orderedAppend
	var arena replyArena
	r1, w1 := socketPair(t)
	r2, w2 := socketPair(t)
	for _, fd := range []int{w1, w2} {
		require.NoError(t, syscall.SetNonblock(fd, true))
	}
	c1 := &client{fd: r1, cmds: []*core.Command{{Cmd: "SET", Args: []string{"k", "first"}}}}
	c2 := &client{fd: r2, cmds: []*core.Command{{Cmd: "SET", Args: []string{"k", "second"}}, {Cmd: "GET", Args: []string{"k"}}}}
	clients[r1], clients[r2] = c1, c2
	require.True(t, accountClient(c1))
	require.True(t, q.admit(c1, mux))
	require.True(t, executeRun(c1, &arena))
	c1.appendOffset = core.AppendOffset()
	ready, err := core.FlushAOFAsync(nil)
	require.NoError(t, err)
	require.False(t, ready)
	require.True(t, accountClient(c2))
	require.True(t, q.admit(c2, mux))
	require.True(t, executeRun(c2, &arena))
	c2.appendOffset = core.AppendOffset()
	require.Equal(t, "+OK\r\n$6\r\nsecond\r\n", string(arena.buf[c2.outStart:c2.outEnd]))
	require.Empty(t, q.gate([]*client{c1, c2}, &arena, mux))
	require.Len(t, q.held, 2)
	for _, fd := range []int{w1, w2} {
		var b [64]byte
		_, err := syscall.Read(fd, b[:])
		require.ErrorIs(t, err, syscall.EAGAIN)
	}
	deadline := time.Now().Add(5 * time.Second)
	for core.AppendReadyOffset() < c1.appendOffset && time.Now().Before(deadline) {
		_, err := core.FlushAOFAsync(nil)
		require.NoError(t, err)
		time.Sleep(time.Millisecond)
	}
	require.GreaterOrEqual(t, core.AppendReadyOffset(), c1.appendOffset)
	require.Less(t, core.AppendReadyOffset(), c2.appendOffset, "the second worker has not been polled yet")
	// A disconnected writer does not cancel its already executed mutation.
	closeClient(c1)
	pool := newIOPool(1)
	defer pool.stop()
	deadline = time.Now().Add(5 * time.Second)
	for len(q.held) > 0 && time.Now().Before(deadline) {
		_, err := q.begin(pool, mux)
		require.NoError(t, err)
		time.Sleep(time.Millisecond)
	}
	require.Empty(t, q.held)
	var b [64]byte
	n, err := syscall.Read(w2, b[:])
	require.NoError(t, err)
	require.Equal(t, "+OK\r\n$6\r\nsecond\r\n", string(b[:n]))
	require.Equal(t, oldRetained, retainedClientBytes)
}

func TestOrderedAdmissionDrainsForUnknownCommandsAndReplyPressure(t *testing.T) {
	core.ResetStores()
	oldClients := clients
	clients = make(map[int]*client)
	t.Cleanup(func() { clients = oldClients })
	old := retainedClientBytes
	defer func() { core.CloseAOF(); retainedClientBytes = old }()
	require.NoError(t, core.OpenAOF(filepath.Join(t.TempDir(), "log")))
	mux := &recordingMonitor{operations: make(map[int]io_multiplexing.Operation)}
	var q orderedAppend
	var arena replyArena
	r, _ := socketPair(t)
	c := &client{fd: r, cmds: []*core.Command{{Cmd: "SET", Args: []string{"k", "v"}}}}
	clients[r] = c
	require.True(t, executeRun(c, &arena))
	_, err := core.FlushAOFAsync(nil)
	require.NoError(t, err)
	r2, _ := socketPair(t)
	blocked := &client{fd: r2, cmds: []*core.Command{{Cmd: "BGREWRITEAOF"}}}
	clients[r2] = blocked
	require.False(t, q.admit(blocked, mux))
	require.True(t, q.drain)
	require.True(t, blocked.appendDeferred)
	require.Len(t, blocked.cmds, 1)
	q.drain = false
	r3, _ := socketPair(t)
	pressured := &client{fd: r3, cmds: []*core.Command{{Cmd: "PING"}}}
	clients[r3] = pressured
	retainedClientBytes = maxRetainedClientBytes - 1
	require.False(t, q.admit(pressured, mux))
	require.Len(t, q.deferred, 2)
	require.Equal(t, io_multiplexing.OpNone, mux.operations[r3])
}

// The aggregate alone lets one class take everything. These check that each
// class refuses on its own, and that the accounting is symmetrical: what a
// connection adds it also gives back.
func TestRetainedBytesAreBoundedPerClassAndInAggregate(t *testing.T) {
	oldClient, oldInput, oldReply := retainedClientBytes, retainedInputBytes, retainedReplyBytes
	t.Cleanup(func() {
		retainedClientBytes, retainedInputBytes, retainedReplyBytes = oldClient, oldInput, oldReply
	})
	retainedClientBytes, retainedInputBytes, retainedReplyBytes = 0, 0, 0

	// A reply just under the class ceiling is accepted, and lands in the reply
	// class rather than the input one.
	c := &client{out: make([]byte, maxRetainedClassBytes-1)}
	require.True(t, accountClient(c))
	require.Equal(t, cap(c.out), c.accountedReply)
	require.Zero(t, c.accountedInput)
	require.Equal(t, retainedReplyBytes, c.accountedReply)
	require.Equal(t, retainedClientBytes, c.accountedReply)

	// Past it, the same connection is refused even though the aggregate has
	// room: three quarters is all one class may hold.
	c.out = make([]byte, maxRetainedClassBytes+1)
	c.outBytes = 0
	require.False(t, accountClient(c))
	require.Less(t, retainedClientBytes, maxRetainedClientBytes,
		"the aggregate still had room, so the class ceiling is what refused")

	// Input is bounded by its own ceiling on the same terms.
	retainedClientBytes, retainedInputBytes, retainedReplyBytes = 0, 0, 0
	in := &client{buf: &connBuffer{data: make([]byte, maxRetainedClassBytes+1)}}
	require.False(t, accountClient(in))
	require.Equal(t, retainedInputBytes, in.accountedInput)
	require.Zero(t, in.accountedReply)
	require.Less(t, retainedClientBytes, maxRetainedClientBytes)
}

func TestReplyFramesUseReplyBudget(t *testing.T) {
	oldClient, oldInput, oldReply := retainedClientBytes, retainedInputBytes, retainedReplyBytes
	t.Cleanup(func() { retainedClientBytes, retainedInputBytes, retainedReplyBytes = oldClient, oldInput, oldReply })
	retainedClientBytes, retainedInputBytes, retainedReplyBytes = maxRetainedClassBytes-4, 0, maxRetainedClassBytes-4
	c := &client{frames: []int{1}, out: []byte("x")}
	require.False(t, accountClient(c), "reply framing must count against the reply ceiling")
	require.Zero(t, c.accountedInput)
	require.Equal(t, 9, c.accountedReply)
	require.False(t, accountClient(c))
	require.Equal(t, 9, c.accountedReply, "re-accounting must not count frames twice")
}
