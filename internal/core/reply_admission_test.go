package core

import (
	"runtime"
	"strings"
	"testing"

	"github.com/brandopakel/keel/internal/data_structure"
	"github.com/stretchr/testify/require"
)

func TestAmplifiedReadsRejectBeforeResponseAllocation(t *testing.T) {
	for _, name := range []string{"MGET", "HMGET", "SRANDMEMBER"} {
		t.Run(name, func(t *testing.T) {
			ResetStores()
			t.Cleanup(ResetStores)
			value := strings.Repeat("x", 1<<20)
			dictStore.Put("string", dictStore.NewObj(value))
			h := data_structure.NewHash()
			h.Set("field", value)
			hashStore.Put("hash", h)
			s := data_structure.NewSet()
			s.Add(value)
			setStore.Put("set", s)
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
			reply := rawReply(t, args[0], args[1:]...)
			runtime.ReadMemStats(&after)
			require.Less(t, len(reply), 1024, "oversized reply must be refused")
			require.Contains(t, string(reply), "reply exceeds")
			require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(256<<10), "reject before allocating the amplified payload")
			require.Equal(t, value, dictStore.Peek("string").Value)
			require.Equal(t, 1, s.Len())
		})
	}
}

func TestReplySizeChecksIncludeFramingAndAvoidOverflow(t *testing.T) {
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
	ResetStores()
	t.Cleanup(ResetStores)
	value := "a\x00\r\nb"
	run(t, "SET", "binary", value)
	run(t, "SET", "empty", "")
	run(t, "HSET", "hash", "binary", value, "empty", "")
	want := Encode([]interface{}{value, "", nil, value}, false)
	require.Equal(t, want, rawReply(t, "MGET", "binary", "empty", "hash", "binary"))
	require.Equal(t, want, rawReply(t, "HMGET", "hash", "binary", "empty", "missing", "binary"))
	run(t, "SADD", "set", value)
	require.Equal(t, Encode([]string{value, value, value}, false), rawReply(t, "SRANDMEMBER", "set", "-3"))
}

func TestRepeatedMemberIndexLimitRejectsBeforeAllocating(t *testing.T) {
	ResetStores()
	t.Cleanup(ResetStores)
	run(t, "SADD", "set", "")
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got := rawReply(t, "SRANDMEMBER", "set", "-16777216")
	runtime.ReadMemStats(&after)
	require.Equal(t, replyTooLarge, got)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(256<<10))
}
