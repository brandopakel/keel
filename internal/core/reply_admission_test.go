package core

import (
	"runtime"
	"strings"
	"testing"

	"github.com/brandopakel/keel/internal/data_structure"
	"github.com/stretchr/testify/require"
)

func TestAmplifiedReadsRejectBeforeResponseAllocation(t *testing.T) {
	// Not parallel: it reads the process's heap statistics, which a test
	// running beside it would move.
	e := newTestEngine(t, Options{})
	for _, name := range []string{"MGET", "HMGET", "SRANDMEMBER"} {
		t.Run(name, func(t *testing.T) {
			e.resetStores()
			value := strings.Repeat("x", 1<<20)
			e.dictStore.Put("string", e.dictStore.NewObj(value))
			h := data_structure.NewHash()
			h.Set("field", value)
			e.hashStore.Put("hash", h)
			s := data_structure.NewSet()
			s.Add(value)
			e.setStore.Put("set", s)
			args := []string{name}
			switch name {
			case "MGET":
				for i := 0; i < 65; i++ {
					args = append(args, "string")
				}
			case "HMGET":
				args = append(args, "hash")
				for i := 0; i < 65; i++ {
					args = append(args, "field")
				}
			case "SRANDMEMBER":
				args = append(args, "set", "-65")
			}
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			reply := rawReplyOn(t, e, args[0], args[1:]...)
			runtime.ReadMemStats(&after)
			require.Less(t, len(reply), 1024, "oversized reply must be refused")
			require.Contains(t, string(reply), "reply exceeds")
			require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(256<<10), "reject before allocating the amplified payload")
			require.Equal(t, value, e.dictStore.Peek("string").Value)
			require.Equal(t, 1, s.Len())
		})
	}
}

func TestReplySizeChecksIncludeFramingAndAvoidOverflow(t *testing.T) {
	t.Parallel()
	for _, length := range []int{0, 9, 10, 99, 100, 1 << 20} {
		framed := length + decimalDigits(length) + 5
		size, fits := addBulkSize(MaxReplyBytes-framed, length)
		require.True(t, fits)
		require.Equal(t, MaxReplyBytes, size)
		_, fits = addBulkSize(MaxReplyBytes-framed+1, length)
		require.False(t, fits)
	}
	_, fits := addBulkSize(16, int(^uint(0)>>1))
	require.False(t, fits)
}

func TestAdmittedLookupRepliesPreserveBinaryEmptyAndNilValues(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	value := "a\x00\r\nb"
	runOn(t, e, "SET", "binary", value)
	runOn(t, e, "SET", "empty", "")
	runOn(t, e, "HSET", "hash", "binary", value, "empty", "")
	want := Encode([]interface{}{value, "", nil, value}, false)
	require.Equal(t, want, rawReplyOn(t, e, "MGET", "binary", "empty", "hash", "binary"))
	require.Equal(t, want, rawReplyOn(t, e, "HMGET", "hash", "binary", "empty", "missing", "binary"))
	runOn(t, e, "SADD", "set", value)
	require.Equal(t, Encode([]string{value, value, value}, false), rawReplyOn(t, e, "SRANDMEMBER", "set", "-3"))
}

func TestRepeatedMemberIndexLimitRejectsBeforeAllocating(t *testing.T) {
	// Not parallel: it reads the process's heap statistics, which a test
	// running beside it would move.
	e := newTestEngine(t, Options{})
	runOn(t, e, "SADD", "set", "")
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got := rawReplyOn(t, e, "SRANDMEMBER", "set", "-16777216")
	runtime.ReadMemStats(&after)
	require.Equal(t, replyTooLarge, got)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(256<<10))
}
