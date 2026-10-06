package core

import (
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCommandAllocationBudgetChecksAggregateAndOverflow(t *testing.T) {
	t.Parallel()
	b := CommandAllocationBudget{Limit: 100}
	b.Begin(40)
	require.True(t, b.Reserve(30))
	require.True(t, b.Reserve(30))
	for _, size := range []int{1, -1, int(^uint(0) >> 1)} {
		require.False(t, b.Reserve(size))
		require.Equal(t, 60, b.Reserved)
	}
	require.Equal(t, 100, b.Peak)
	b.End()
	b.Begin(10)
	require.True(t, b.Reserve(90), "previous run released its reservations")
	require.Equal(t, uint64(3), b.Refusals)
}

func TestCommandAllocationPeakIncludesExistingBuffers(t *testing.T) {
	t.Parallel()
	b := CommandAllocationBudget{Limit: 100}
	b.Begin(64)
	require.Equal(t, 64, b.Peak, "a command need not make a reservation")
	require.False(t, b.Reserve(37))
	require.Equal(t, 64, b.Peak, "refused memory was never allocated")
	require.True(t, b.Reserve(10))
	b.ObserveRetained(80)
	require.Equal(t, 90, b.Peak, "refresh preserves current reservations")
	require.Equal(t, 10, b.Reserved)
	b.End()
	b.Begin(20)
	require.Equal(t, 90, b.Peak, "peak survives release")
}

func TestCommandAllocationRefusalPreservesWritesAndReplay(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	old := e.commandAllocations
	t.Cleanup(func() { e.commandAllocations = old; e.resetStores() })
	value := strings.Repeat("v", 16<<10)
	path := withAOFOn(t, e, func() {
		runOn(t, e, "SET", "string", value)
		runOn(t, e, "RPUSH", "list", value, value)
		runOn(t, e, "SADD", "set", value, "second")
		runOn(t, e, "ZADD", "zset", "1", value, "2", "second")
		for _, command := range [][]string{
			{"SET", "string", "replacement", "GET"},
			{"LPOP", "list", "2"}, {"RPOP", "list", "2"},
			{"SPOP", "set", "2"}, {"ZPOPMIN", "zset", "2"}, {"ZPOPMAX", "zset", "2"},
		} {
			e.commandAllocations = &CommandAllocationBudget{Limit: 32 << 10}
			got := rawReplyOn(t, e, command[0], command[1:]...)
			require.Equal(t, allocationPressure, got, "%s", command[0])
			require.Positive(t, e.commandAllocations.Refusals)
			e.commandAllocations = nil
			require.Equal(t, value, runOn(t, e, "GET", "string"))
			require.EqualValues(t, 2, runOn(t, e, "LLEN", "list"))
			require.EqualValues(t, 2, runOn(t, e, "SCARD", "set"))
			require.EqualValues(t, 2, runOn(t, e, "ZCARD", "zset"))
		}
		runOn(t, e, "SET", "after", "accepted")
	})
	for replay := 0; replay < 2; replay++ {
		restartOn(t, e, path)
		require.Equal(t, value, runOn(t, e, "GET", "string"))
		require.Equal(t, "accepted", runOn(t, e, "GET", "after"))
		require.Equal(t, []interface{}{value, value}, runOn(t, e, "LRANGE", "list", "0", "-1"))
		require.ElementsMatch(t, []interface{}{value, "second"}, runOn(t, e, "SMEMBERS", "set"))
		require.Equal(t, []interface{}{value, "1", "second", "2"}, runOn(t, e, "ZRANGE", "zset", "0", "-1", "WITHSCORES"))
	}
}

func TestCommandAllocationAdmissionRejectsBeforeLargeReadBuffers(t *testing.T) {
	// Not parallel: it reads the process's heap statistics, which a test
	// running beside it would move.
	e := newTestEngine(t, Options{})
	old := e.commandAllocations
	t.Cleanup(func() { e.commandAllocations = old; e.resetStores() })
	value := strings.Repeat("x", 4<<20)
	runOn(t, e, "SET", "large", value)
	runOn(t, e, "HSET", "hash", "field", value)
	runOn(t, e, "SADD", "set", value)
	runOn(t, e, "RPUSH", "list", value)
	for _, command := range [][]string{
		{"GET", "large"}, {"MGET", "large", "large"},
		{"HGET", "hash", "field"}, {"HMGET", "hash", "field", "field"},
		{"HGETALL", "hash"}, {"SMEMBERS", "set"},
		{"SRANDMEMBER", "set", "-1000000"}, {"LINDEX", "list", "0"},
		{"KEEL.DUMP", "large"}, {"LCS", "large", "large", "LEN"},
	} {
		e.commandAllocations = &CommandAllocationBudget{Limit: 1 << 20, Retained: 900 << 10}
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		got := rawReplyOn(t, e, command[0], command[1:]...)
		runtime.ReadMemStats(&after)
		// LCS's configured CPU gate may refuse before reaching allocation.
		require.Equal(t, byte('-'), got[0], "%s", command[0])
		require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(128<<10), "%s", command[0])
	}
	e.commandAllocations = nil
	require.Equal(t, value, runOn(t, e, "GET", "large"))
}

func TestKeysAndScanRefuseOversizedNamesBeforeEncoding(t *testing.T) {
	// Not parallel: it reads the process's heap statistics, which a test
	// running beside it would move.
	e := newTestEngine(t, Options{})
	key := strings.Repeat("k", MaxReplyBytes)
	e.dictStore.Put(key, e.dictStore.NewObj("v"))
	for _, command := range [][]string{{"KEYS", "*"}, {"SCAN", "0", "COUNT", "100"}} {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		got := rawReplyOn(t, e, command[0], command[1:]...)
		runtime.ReadMemStats(&after)
		require.Equal(t, replyTooLarge, got)
		require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(128<<10))
	}
}

func TestCommandWorkspaceAndReplyShareReservation(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	old := e.commandAllocations
	t.Cleanup(func() { e.commandAllocations = old; e.resetStores() })
	for i := 0; i < 1000; i++ {
		runOn(t, e, "GEOADD", "geo", "0", "0", strings.Repeat("m", 100)+strconv.Itoa(i))
	}
	e.commandAllocations = &CommandAllocationBudget{Limit: 128 << 10}
	got := rawReplyOn(t, e, "GEOSEARCH", "geo", "FROMLONLAT", "0", "0", "BYRADIUS", "1", "km", "COUNT", "1000")
	require.Equal(t, allocationPressure, got)
	require.Greater(t, e.commandAllocations.Reserved, 48000, "point storage fits but combined encoded reply does not")
	require.Less(t, e.commandAllocations.Reserved, e.commandAllocations.Limit)
	e.commandAllocations = nil
	runOn(t, e, "SET", "a", strings.Repeat("ab", 500))
	runOn(t, e, "SET", "b", strings.Repeat("ba", 500))
	for _, options := range [][]string{{"LEN"}, {}, {"IDX", "WITHMATCHLEN"}} {
		e.commandAllocations = &CommandAllocationBudget{Limit: 8192}
		got := rawReplyOn(t, e, "LCS", append([]string{"a", "b"}, options...)...)
		require.Equal(t, allocationPressure, got)
	}
	e.commandAllocations = &CommandAllocationBudget{Limit: 2 << 20}
	require.EqualValues(t, 999, runOn(t, e, "LCS", "a", "b", "LEN"))
	e.commandAllocations.End()
	require.Equal(t, strings.Repeat("ba", 499)+"b", runOn(t, e, "LCS", "a", "b"))
}

func TestCommandRemovalReservesExistingLogGrowth(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	old := e.commandAllocations
	t.Cleanup(func() { e.commandAllocations = old; e.resetStores() })
	path := withAOFOn(t, e, func() {
		runOn(t, e, "SADD", "set", "member")
		runOn(t, e, "SET", "padding", strings.Repeat("p", 1<<20))
		// Fill the current backing allocation so even this tiny removal would
		// force replacement of a large append buffer.
		e.aof.buf = append(make([]byte, 0, len(e.aof.buf)), e.aof.buf...)
		before := len(e.aof.buf)
		e.commandAllocations = &CommandAllocationBudget{Limit: 2 << 20, Retained: cap(e.aof.buf)}
		require.Equal(t, allocationPressure, rawReplyOn(t, e, "SPOP", "set"))
		require.Equal(t, before, len(e.aof.buf), "refused command must not append a partial record")
		e.commandAllocations = nil
		require.EqualValues(t, 1, runOn(t, e, "SCARD", "set"))
	})
	restartOn(t, e, path)
	require.Equal(t, []interface{}{"member"}, runOn(t, e, "SMEMBERS", "set"))
}

func TestCommandReplyClassCanRefuseBeforeAggregateLimit(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	old := e.commandAllocations
	t.Cleanup(func() { e.commandAllocations = old; e.resetStores() })
	runOn(t, e, "SET", "value", strings.Repeat("v", 1<<20))
	e.commandAllocations = &CommandAllocationBudget{Limit: 16 << 20, ReplyLimit: 4 << 20, ReplyRetained: 2 << 20}
	require.Equal(t, allocationPressure, rawReplyOn(t, e, "GET", "value"))
	require.Zero(t, e.commandAllocations.Reserved)
	require.Zero(t, e.commandAllocations.ReplyReserved)
	e.commandAllocations.ReplyRetained = 0
	require.Equal(t, byte('$'), rawReplyOn(t, e, "GET", "value")[0])
	require.Positive(t, e.commandAllocations.ReplyReserved)
}
