package core

import (
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCommandAllocationBudgetChecksAggregateAndOverflow(t *testing.T) {
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

func TestCommandAllocationRefusalPreservesWritesAndReplay(t *testing.T) {
	old := CommandAllocations
	t.Cleanup(func() { CommandAllocations = old; ResetStores() })
	value := strings.Repeat("v", 16<<10)
	path := withAOF(t, func() {
		run(t, "SET", "string", value)
		run(t, "RPUSH", "list", value, value)
		run(t, "SADD", "set", value, "second")
		run(t, "ZADD", "zset", "1", value, "2", "second")
		for _, command := range [][]string{
			{"SET", "string", "replacement", "GET"},
			{"LPOP", "list", "2"}, {"RPOP", "list", "2"},
			{"SPOP", "set", "2"}, {"ZPOPMIN", "zset", "2"}, {"ZPOPMAX", "zset", "2"},
		} {
			CommandAllocations = &CommandAllocationBudget{Limit: 32 << 10}
			got := rawReply(t, command[0], command[1:]...)
			require.Equal(t, allocationPressure, got, "%s", command[0])
			require.Positive(t, CommandAllocations.Refusals)
			CommandAllocations = nil
			require.Equal(t, value, run(t, "GET", "string"))
			require.EqualValues(t, 2, run(t, "LLEN", "list"))
			require.EqualValues(t, 2, run(t, "SCARD", "set"))
			require.EqualValues(t, 2, run(t, "ZCARD", "zset"))
		}
		run(t, "SET", "after", "accepted")
	})
	for replay := 0; replay < 2; replay++ {
		restart(t, path)
		require.Equal(t, value, run(t, "GET", "string"))
		require.Equal(t, "accepted", run(t, "GET", "after"))
		require.Equal(t, []interface{}{value, value}, run(t, "LRANGE", "list", "0", "-1"))
		require.ElementsMatch(t, []interface{}{value, "second"}, run(t, "SMEMBERS", "set"))
		require.Equal(t, []interface{}{value, "1", "second", "2"}, run(t, "ZRANGE", "zset", "0", "-1", "WITHSCORES"))
	}
}

func TestCommandAllocationAdmissionRejectsBeforeLargeReadBuffers(t *testing.T) {
	ResetStores()
	old := CommandAllocations
	t.Cleanup(func() { CommandAllocations = old; ResetStores() })
	value := strings.Repeat("x", 4<<20)
	run(t, "SET", "large", value)
	run(t, "HSET", "hash", "field", value)
	run(t, "SADD", "set", value)
	run(t, "RPUSH", "list", value)
	for _, command := range [][]string{
		{"GET", "large"}, {"MGET", "large", "large"},
		{"HGET", "hash", "field"}, {"HMGET", "hash", "field", "field"},
		{"HGETALL", "hash"}, {"SMEMBERS", "set"},
		{"SRANDMEMBER", "set", "-1000000"}, {"LINDEX", "list", "0"},
		{"KEEL.DUMP", "large"}, {"LCS", "large", "large", "LEN"},
	} {
		CommandAllocations = &CommandAllocationBudget{Limit: 1 << 20, Retained: 900 << 10}
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		got := rawReply(t, command[0], command[1:]...)
		runtime.ReadMemStats(&after)
		// LCS's configured CPU gate may refuse before reaching allocation.
		require.Equal(t, byte('-'), got[0], "%s", command[0])
		require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(128<<10), "%s", command[0])
	}
	CommandAllocations = nil
	require.Equal(t, value, run(t, "GET", "large"))
}

func TestKeysAndScanRefuseOversizedNamesBeforeEncoding(t *testing.T) {
	ResetStores()
	t.Cleanup(ResetStores)
	key := strings.Repeat("k", MaxReplyBytes)
	dictStore.Put(key, dictStore.NewObj("v"))
	for _, command := range [][]string{{"KEYS", "*"}, {"SCAN", "0", "COUNT", "100"}} {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		got := rawReply(t, command[0], command[1:]...)
		runtime.ReadMemStats(&after)
		require.Equal(t, replyTooLarge, got)
		require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(128<<10))
	}
}

func TestCommandWorkspaceAndReplyShareReservation(t *testing.T) {
	ResetStores()
	old := CommandAllocations
	t.Cleanup(func() { CommandAllocations = old; ResetStores() })
	for i := 0; i < 1000; i++ {
		run(t, "GEOADD", "geo", "0", "0", strings.Repeat("m", 100)+strconv.Itoa(i))
	}
	CommandAllocations = &CommandAllocationBudget{Limit: 128 << 10}
	got := rawReply(t, "GEOSEARCH", "geo", "FROMLONLAT", "0", "0", "BYRADIUS", "1", "km", "COUNT", "1000")
	require.Equal(t, allocationPressure, got)
	require.Greater(t, CommandAllocations.Reserved, 48000, "point storage fits but combined encoded reply does not")
	require.Less(t, CommandAllocations.Reserved, CommandAllocations.Limit)
	CommandAllocations = nil
	run(t, "SET", "a", strings.Repeat("ab", 500))
	run(t, "SET", "b", strings.Repeat("ba", 500))
	for _, options := range [][]string{{"LEN"}, {}, {"IDX", "WITHMATCHLEN"}} {
		CommandAllocations = &CommandAllocationBudget{Limit: 8192}
		got := rawReply(t, "LCS", append([]string{"a", "b"}, options...)...)
		require.Equal(t, allocationPressure, got)
	}
	CommandAllocations = &CommandAllocationBudget{Limit: 2 << 20}
	require.EqualValues(t, 999, run(t, "LCS", "a", "b", "LEN"))
	CommandAllocations.End()
	require.Equal(t, strings.Repeat("ba", 499)+"b", run(t, "LCS", "a", "b"))
}

func TestCommandRemovalReservesExistingLogGrowth(t *testing.T) {
	old := CommandAllocations
	t.Cleanup(func() { CommandAllocations = old; ResetStores() })
	path := withAOF(t, func() {
		run(t, "SADD", "set", "member")
		run(t, "SET", "padding", strings.Repeat("p", 1<<20))
		// Fill the current backing allocation so even this tiny removal would
		// force replacement of a large append buffer.
		aof.buf = append(make([]byte, 0, len(aof.buf)), aof.buf...)
		before := len(aof.buf)
		CommandAllocations = &CommandAllocationBudget{Limit: 2 << 20, Retained: cap(aof.buf)}
		require.Equal(t, allocationPressure, rawReply(t, "SPOP", "set"))
		require.Equal(t, before, len(aof.buf), "refused command must not append a partial record")
		CommandAllocations = nil
		require.EqualValues(t, 1, run(t, "SCARD", "set"))
	})
	restart(t, path)
	require.Equal(t, []interface{}{"member"}, run(t, "SMEMBERS", "set"))
}

func TestCommandReplyClassCanRefuseBeforeAggregateLimit(t *testing.T) {
	ResetStores()
	old := CommandAllocations
	t.Cleanup(func() { CommandAllocations = old; ResetStores() })
	run(t, "SET", "value", strings.Repeat("v", 1<<20))
	CommandAllocations = &CommandAllocationBudget{Limit: 16 << 20, ReplyLimit: 4 << 20, ReplyRetained: 2 << 20}
	require.Equal(t, allocationPressure, rawReply(t, "GET", "value"))
	require.Zero(t, CommandAllocations.Reserved)
	require.Zero(t, CommandAllocations.ReplyReserved)
	CommandAllocations.ReplyRetained = 0
	require.Equal(t, byte('$'), rawReply(t, "GET", "value")[0])
	require.Positive(t, CommandAllocations.ReplyReserved)
}
