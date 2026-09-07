package core

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLargeCollectionRewriteYieldsAndReconcilesMutation(t *testing.T) {
	for _, kind := range []string{"set", "zset", "hash"} {
		for _, mutation := range []string{"update", "replace", "delete", "sample"} {
			if kind != "set" && mutation == "sample" {
				continue
			}
			t.Run(kind+"/"+mutation, func(t *testing.T) {
				ResetStores()
				path := filepath.Join(t.TempDir(), "log")
				require.NoError(t, OpenAOF(path))
				defer CloseAOF()
				for i := 0; i < 2000; i++ {
					member := fmt.Sprintf("member-%04d", i)
					if kind == "set" {
						run(t, "SADD", "large", member)
					} else if kind == "hash" {
						run(t, "HSET", "large", member, strconv.Itoa(i))
					} else {
						run(t, "ZADD", "large", strconv.Itoa(i), member)
					}
				}
				run(t, "PEXPIRE", "large", "60000")
				require.NoError(t, StartRewrite())
				require.NoError(t, AdvanceRewrite())
				require.True(t, rewrite.collectionActive)
				require.LessOrEqual(t, rewrite.collectionPos, 256)
				switch mutation {
				case "update":
					if kind == "set" {
						run(t, "SADD", "large", "new")
						run(t, "SREM", "large", "member-0000")
					} else if kind == "hash" {
						run(t, "HSET", "large", "member-0000", "9999")
						run(t, "HDEL", "large", "member-0001")
					} else {
						run(t, "ZADD", "large", "9999", "member-0000")
						run(t, "ZREM", "large", "member-0001")
					}
				case "replace":
					run(t, "DEL", "large")
					run(t, "SET", "large", "replacement")
				case "delete":
					run(t, "DEL", "large")
				case "sample":
					run(t, "SRANDMEMBER", "large", "1000")
				}
				require.False(t, rewrite.collectionActive)
				require.Nil(t, rewrite.hashCursor)
				var want interface{}
				if mutation == "update" || mutation == "sample" {
					if kind == "set" {
						want = run(t, "SMEMBERS", "large")
					} else if kind == "hash" {
						want = hashRewriteState(t, "large")
					} else {
						want = run(t, "ZRANGE", "large", "0", "-1", "WITHSCORES")
					}
				}
				for i := 0; rewrite.active && i < 100; i++ {
					require.NoError(t, FlushAOF())
				}
				require.False(t, rewrite.active)
				require.Equal(t, 1, aof.rewrites)
				require.NoError(t, CloseAOF())
				for i := 0; i < 2; i++ {
					ResetStores()
					_, err := LoadAOF(path)
					require.NoError(t, err)
					switch mutation {
					case "replace":
						require.Equal(t, "replacement", run(t, "GET", "large"))
					case "delete":
						require.Equal(t, int64(0), run(t, "EXISTS", "large"))
					default:
						if kind == "set" {
							require.ElementsMatch(t, want, run(t, "SMEMBERS", "large"))
						} else if kind == "hash" {
							require.Equal(t, want, hashRewriteState(t, "large"))
						} else {
							require.Equal(t, want, run(t, "ZRANGE", "large", "0", "-1", "WITHSCORES"))
						}
						require.Greater(t, run(t, "PTTL", "large").(int64), int64(0))
					}
				}
			})
		}
	}
}

func TestCollectionRewriteHonorsByteBudgetAndOversizedMemberMakesProgress(t *testing.T) {
	for _, kind := range []string{"set", "zset", "list", "hash"} {
		t.Run(kind, func(t *testing.T) {
			ResetStores()
			require.NoError(t, OpenAOF(filepath.Join(t.TempDir(), "log")))
			defer CloseAOF()
			for i := 0; i < 100; i++ {
				value := strconv.Itoa(i) + strings.Repeat("x", 4096)
				switch kind {
				case "hash":
					run(t, "HSET", "large", strconv.Itoa(i), value)
				case "set":
					run(t, "SADD", "large", value)
				case "list":
					run(t, "RPUSH", "large", value)
				case "zset":
					run(t, "ZADD", "large", strconv.Itoa(i), value)
				}
			}
			// One member exceeds the target. It must be emitted alone, never strand
			// the cursor or silently truncate the value.
			huge := strings.Repeat("y", 100000)
			switch kind {
			case "hash":
				run(t, "HSET", "large", "huge", huge)
			case "set":
				run(t, "SADD", "large", huge)
			case "list":
				run(t, "RPUSH", "large", huge)
			case "zset":
				run(t, "ZADD", "large", "-1", huge)
			}
			require.NoError(t, StartRewrite())
			chunks := 0
			for rewrite.active && chunks < 50 {
				before := rewrite.written
				require.NoError(t, AdvanceRewrite())
				if rewrite.active {
					require.LessOrEqual(t, rewrite.written-before, int64(len(huge)+256))
				}
				chunks++
			}
			require.Greater(t, chunks, 5)
			require.False(t, rewrite.active)
			require.Equal(t, 1, aof.rewrites)
		})
	}
}

func hashRewriteState(t *testing.T, key string) map[string]string {
	t.Helper()
	h, ok := hashStore.Peek(key)
	require.True(t, ok)
	fields, values := h.Entries()
	out := make(map[string]string, len(fields))
	for i, field := range fields {
		out[field] = values[i]
	}
	return out
}
