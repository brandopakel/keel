package core

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/brandopakel/keel/internal/data_structure"
	"github.com/stretchr/testify/require"
)

func TestLargeCollectionRepliesRejectBeforeAllocationOrRemoval(t *testing.T) {
	cases := []struct {
		name, kind string
		args       []string
	}{
		{"hash-all", "hash", []string{"HGETALL", "large"}},
		{"hash-keys", "hash", []string{"HKEYS", "large"}},
		{"hash-values", "hash", []string{"HVALS", "large"}},
		{"list-range", "list", []string{"LRANGE", "large", "0", "-1"}},
		{"list-pop-front", "list", []string{"LPOP", "large", "65"}},
		{"list-pop-back", "list", []string{"RPOP", "large", "65"}},
		{"set-members", "set", []string{"SMEMBERS", "large"}},
		{"set-distinct", "set", []string{"SRANDMEMBER", "large", "65"}},
		{"set-pop", "set", []string{"SPOP", "large", "65"}},
		{"zset-range", "zset", []string{"ZRANGE", "large", "0", "-1", "WITHSCORES"}},
		{"zset-score", "zset", []string{"ZRANGEBYSCORE", "large", "-inf", "+inf"}},
		{"zset-reverse-score", "zset", []string{"ZREVRANGEBYSCORE", "large", "+inf", "-inf", "WITHSCORES"}},
		{"zset-pop-min", "zset", []string{"ZPOPMIN", "large", "65"}},
		{"zset-pop-max", "zset", []string{"ZPOPMAX", "large", "65"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ResetStores()
			t.Cleanup(ResetStores)
			h, l, s, z := data_structure.NewHash(), data_structure.NewList(), data_structure.NewSet(), data_structure.CreateZSet()
			value := strings.Repeat("x", 1<<20)
			for i := 0; i < 65; i++ {
				v := fmt.Sprintf("%03d", i) + value
				switch tc.kind {
				case "hash":
					h.Set(v, v)
				case "list":
					l.PushBack(v)
				case "set":
					s.Add(v)
				case "zset":
					z.Add(float64(i), v, 0)
				}
			}
			switch tc.kind {
			case "hash":
				hashStore.Put("large", h)
			case "list":
				listStore.Put("large", l)
			case "set":
				setStore.Put("large", s)
			case "zset":
				zsetStore.Put("large", z)
			}
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			got := rawReply(t, tc.args[0], tc.args[1:]...)
			runtime.ReadMemStats(&after)
			require.Less(t, len(got), 1024, "oversized payload must not be built")
			require.Equal(t, replyTooLarge, got)
			require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(256<<10))
			switch tc.kind {
			case "hash":
				require.Equal(t, 65, h.Len())
			case "list":
				require.Equal(t, 65, l.Len())
			case "set":
				require.Equal(t, 65, s.Len())
			case "zset":
				require.Equal(t, 65, z.Len())
			}
		})
	}
}

func TestPopRecordAdmissionIncludesLargeKeyBeforeRemoval(t *testing.T) {
	for _, command := range []string{"SPOP", "ZPOPMIN"} {
		t.Run(command, func(t *testing.T) {
			ResetStores()
			t.Cleanup(ResetStores)
			key := strings.Repeat("k", 8<<20)
			s, z := data_structure.NewSet(), data_structure.CreateZSet()
			for i := 0; i < 60; i++ {
				value := fmt.Sprintf("%03d", i) + strings.Repeat("v", 1<<20)
				if command == "SPOP" {
					s.Add(value)
				} else {
					z.Add(float64(i), value, 0)
				}
			}
			if command == "SPOP" {
				setStore.Put(key, s)
			} else {
				zsetStore.Put(key, z)
			}
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			got := rawReply(t, command, key, "60")
			runtime.ReadMemStats(&after)
			require.Equal(t, replyTooLarge, got, "reply fits but canonical removal record exceeds limit")
			require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(256<<10))
			if command == "SPOP" {
				require.Equal(t, 60, s.Len())
			} else {
				require.Equal(t, 60, z.Len())
			}
		})
	}
}
