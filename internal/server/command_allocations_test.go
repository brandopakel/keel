package server

import (
	"runtime"
	"strings"
	"testing"

	"github.com/brandopakel/keel/internal/core"
	"github.com/stretchr/testify/require"
)

func TestCommandReservationsIncludeOtherClientsAndReleaseAfterDrain(t *testing.T) {
	core.ResetStores()
	oldBudget := core.CommandAllocations
	oldTotal, oldInput, oldReply := retainedClientBytes, retainedInputBytes, retainedReplyBytes
	t.Cleanup(func() {
		core.CommandAllocations = oldBudget
		retainedClientBytes, retainedInputBytes, retainedReplyBytes = oldTotal, oldInput, oldReply
		core.ResetStores()
	})
	retainedClientBytes, retainedInputBytes, retainedReplyBytes = 0, 0, 0
	var sink replyBuffer
	value := strings.Repeat("x", 2<<20)
	responseRw(&core.Command{Cmd: "SET", Args: []string{"large", value}}, &sink)
	core.CommandAllocations = &core.CommandAllocationBudget{Limit: 8 << 20}
	slow := &client{out: make([]byte, 4<<20)}
	require.True(t, accountClient(slow))
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	c := &client{cmds: []*core.Command{{Cmd: "GET", Args: []string{"large"}}}}
	var arena replyArena
	require.True(t, executeRun(c, &arena))
	runtime.ReadMemStats(&after)
	require.Contains(t, string(c.out), "temporary command allocation budget exhausted")
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(128<<10))
	require.Equal(t, 0, core.CommandAllocations.Reserved)
	// Releasing the old reply restores headroom; the same GET can then execute.
	slow.out, slow.outBytes = nil, 0
	require.True(t, accountClient(slow))
	c = &client{cmds: []*core.Command{{Cmd: "GET", Args: []string{"large"}}}}
	require.True(t, executeRun(c, &arena))
	require.Equal(t, byte('$'), c.out[0])
	require.Greater(t, len(c.out), len(value))
	require.LessOrEqual(t, core.CommandAllocations.Peak, 8<<20)
}
