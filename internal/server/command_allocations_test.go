package server

import (
	"runtime"
	"strings"
	"testing"

	"github.com/brandopakel/keel/internal/core"
	"github.com/stretchr/testify/require"
)

func TestCommandReservationsIncludeOtherClientsAndReleaseAfterDrain(t *testing.T) {
	e := newTestEngine(t, core.Options{})
	oldTotal, oldInput, oldReply := retainedClientBytes, retainedInputBytes, retainedReplyBytes
	t.Cleanup(func() {
		retainedClientBytes, retainedInputBytes, retainedReplyBytes = oldTotal, oldInput, oldReply
	})
	retainedClientBytes, retainedInputBytes, retainedReplyBytes = 0, 0, 0
	var sink replyBuffer
	value := strings.Repeat("x", 2<<20)
	responseRw(e, &core.Command{Cmd: "SET", Args: []string{"large", value}}, &sink)
	e.SetCommandAllocations(&core.CommandAllocationBudget{Limit: 8 << 20})
	slow := &client{out: make([]byte, 4<<20)}
	require.True(t, accountClient(slow))
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	c := &client{cmds: []*core.Command{{Cmd: "GET", Args: []string{"large"}}}, engine: e}
	var arena replyArena
	require.True(t, executeRun(c, &arena))
	runtime.ReadMemStats(&after)
	require.Contains(t, string(c.out), "temporary command allocation budget exhausted")
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(128<<10))
	require.Equal(t, 0, e.CommandAllocations().Reserved)
	// Releasing the old reply restores headroom; the same GET can then execute.
	slow.out, slow.outBytes = nil, 0
	require.True(t, accountClient(slow))
	c = &client{cmds: []*core.Command{{Cmd: "GET", Args: []string{"large"}}}, engine: e}
	require.True(t, executeRun(c, &arena))
	require.Equal(t, byte('$'), c.out[0])
	require.Greater(t, len(c.out), len(value))
	require.LessOrEqual(t, e.CommandAllocations().Peak, 8<<20)
}
