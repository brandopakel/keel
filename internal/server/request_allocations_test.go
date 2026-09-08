package server

import (
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/brandopakel/keel/internal/core"
	"github.com/stretchr/testify/require"
)

func TestRequestReservationsAreSharedAcrossWorkers(t *testing.T) {
	var budget requestAllocationBudget
	budget.begin(100, 20, 1100, 2000)
	var accepted atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if budget.reserve(10) {
					accepted.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	require.EqualValues(t, 100, accepted.Load())
	require.EqualValues(t, 1000, budget.used.Load())
	budget.end()
	require.EqualValues(t, 1100, budget.peak)
	require.Zero(t, budget.used.Load())
	budget.begin(0, 0, 2000, 50)
	require.True(t, budget.reserve(50))
	require.False(t, budget.reserve(1), "the input class also leaves room for replies")
}

func TestRequestReservationRefusesBufferGrowthBeforeAllocation(t *testing.T) {
	r, _ := socketPair(t)
	require.NoError(t, syscall.SetNonblock(r, true))
	prefix := []byte("*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$1000000\r\n")
	c := &client{fd: r, buf: &connBuffer{data: append([]byte(nil), prefix...)}}
	var budget requestAllocationBudget
	budget.begin(0, 0, 1024, 1024)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	cmds, err := c.readCommandsReserved(testScratch, &budget)
	runtime.ReadMemStats(&after)
	require.ErrorIs(t, err, core.ErrRequestAllocation)
	require.Nil(t, cmds)
	require.Nil(t, c.buf)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(16<<10))
}

func TestRequestReservationRefusalAndNextPhaseRecovery(t *testing.T) {
	wire := encodeCmd("SET", "key", strings.Repeat("v", 1<<20))
	c := &client{bufferedReady: true, buf: &connBuffer{data: wire}}
	var budget requestAllocationBudget
	budget.begin(len(wire), len(wire), len(wire)+1024, 8<<20)
	cmds, err := c.readCommandsReserved(testScratch, &budget)
	require.ErrorIs(t, err, core.ErrRequestAllocation)
	require.Nil(t, cmds)
	require.Nil(t, c.buf)
	require.Positive(t, budget.refusals.Load())
	budget.end()
	budget.begin(len(wire), len(wire), 8<<20, 6<<20)
	c = &client{bufferedReady: true, buf: &connBuffer{data: wire}}
	cmds, err = c.readCommandsReserved(testScratch, &budget)
	require.NoError(t, err)
	require.Len(t, cmds, 1)
	require.Equal(t, "key", cmds[0].Args[0])
	require.Len(t, cmds[0].Args[1], 1<<20)
	budget.end()
	require.LessOrEqual(t, budget.peak, int64(8<<20))
}

func TestRequestReaderPoolSharesAdmissionAndRecovers(t *testing.T) {
	p := newIOPool(4)
	defer p.stop()
	var budget requestAllocationBudget
	p.requestBudget = &budget
	cs := make([]*client, 16) // Exceeds the threshold for all four I/O threads.
	for i := range cs {
		cs[i] = &client{bufferedReady: true, buf: &connBuffer{data: encodeCmd("SET", "key", strings.Repeat("v", 64<<10))}}
	}
	budget.begin(0, 0, 256<<10, 256<<10)
	p.run(cs, false)
	accepted, refused := 0, 0
	for _, c := range cs {
		if c.err != nil {
			require.ErrorIs(t, c.err, core.ErrRequestAllocation)
			require.Nil(t, c.buf)
			require.Empty(t, c.cmds)
			refused++
		} else {
			require.Len(t, c.cmds, 1)
			require.Len(t, c.cmds[0].Args[1], 64<<10)
			accepted++
		}
	}
	require.Positive(t, accepted)
	require.Positive(t, refused)
	require.Equal(t, len(cs), accepted+refused)
	require.LessOrEqual(t, budget.used.Load(), int64(256<<10))
	budget.end()
	// Joined workers cannot carry a stale allowance into the next phase.
	budget.begin(0, 0, 2<<20, 2<<20)
	for i := range cs {
		cs[i] = &client{bufferedReady: true, buf: &connBuffer{data: encodeCmd("SET", "key", strings.Repeat("v", 64<<10))}}
	}
	p.run(cs, false)
	for _, c := range cs {
		require.NoError(t, c.err)
		require.Len(t, c.cmds, 1)
	}
	budget.end()
}

func TestRequestBudgetInvalidOrExhaustedOwnershipFailsClosed(t *testing.T) {
	for _, state := range [][4]int{{-1, 0, 100, 100}, {0, -1, 100, 100}, {101, 0, 100, 100}, {0, 101, 100, 100}, {0, 0, -1, 100}, {0, 0, 100, -1}} {
		var budget requestAllocationBudget
		budget.begin(state[0], state[1], state[2], state[3])
		require.False(t, budget.reserve(1), "state %v", state)
		budget.end()
	}
}
