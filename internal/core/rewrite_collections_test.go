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
	t.Parallel()
	e := newTestEngine(t, Options{})
	for _, kind := range []string{"set", "zset", "hash"} {
		for _, mutation := range []string{"update", "replace", "delete", "sample", "shrink-regrow"} {
			if kind != "set" && mutation == "sample" {
				continue
			}
			if kind != "hash" && mutation == "shrink-regrow" {
				continue
			}
			t.Run(kind+"/"+mutation, func(t *testing.T) {
				e.resetStores()
				path := filepath.Join(t.TempDir(), "log")
				require.NoError(t, e.OpenAOF(path))
				defer e.CloseAOF()
				for i := 0; i < 2000; i++ {
					member := fmt.Sprintf("member-%04d", i)
					if kind == "set" {
						runOn(t, e, "SADD", "large", member)
					} else if kind == "hash" {
						runOn(t, e, "HSET", "large", member, strconv.Itoa(i))
					} else {
						runOn(t, e, "ZADD", "large", strconv.Itoa(i), member)
					}
				}
				runOn(t, e, "PEXPIRE", "large", "60000")
				require.NoError(t, e.StartRewrite())
				require.NoError(t, e.AdvanceRewrite())
				waitForRewriteSyncOn(t, e)
				require.True(t, e.rewrite.collectionActive)
				require.LessOrEqual(t, e.rewrite.collectionPos, 256)
				switch mutation {
				case "update":
					if kind == "set" {
						runOn(t, e, "SADD", "large", "new")
						runOn(t, e, "SREM", "large", "member-0000")
					} else if kind == "hash" {
						runOn(t, e, "HSET", "large", "member-0000", "9999")
						runOn(t, e, "HDEL", "large", "member-0001")
					} else {
						runOn(t, e, "ZADD", "large", "9999", "member-0000")
						runOn(t, e, "ZREM", "large", "member-0001")
					}
				case "replace":
					runOn(t, e, "DEL", "large")
					runOn(t, e, "SET", "large", "replacement")
				case "delete":
					runOn(t, e, "DEL", "large")
				case "sample":
					runOn(t, e, "SRANDMEMBER", "large", "1000")
				case "shrink-regrow":
					// Invalidate the active map cursor while bounded hash leaves
					// merge, demote to one leaf, then split again with new fields.
					for i := 10; i < 2000; i++ {
						require.Equal(t, int64(1), runOn(t, e, "HDEL", "large", fmt.Sprintf("member-%04d", i)))
					}
					for i := 0; i < 2000; i++ {
						require.Equal(t, int64(1), runOn(t, e, "HSET", "large", fmt.Sprintf("new-%04d", i), strconv.Itoa(i)))
					}
					require.Equal(t, int64(2010), runOn(t, e, "HLEN", "large"))
				}
				require.False(t, e.rewrite.collectionActive)
				require.Nil(t, e.rewrite.hashCursor)
				var want interface{}
				if mutation == "update" || mutation == "sample" || mutation == "shrink-regrow" {
					if kind == "set" {
						want = runOn(t, e, "SMEMBERS", "large")
					} else if kind == "hash" {
						want = hashRewriteState(t, e, "large")
					} else {
						want = runOn(t, e, "ZRANGE", "large", "0", "-1", "WITHSCORES")
					}
				}
				for i := 0; e.rewrite.active && i < 100; i++ {
					require.NoError(t, e.FlushAOF())
					waitForRewriteSyncOn(t, e)
				}
				require.False(t, e.rewrite.active)
				require.Equal(t, 1, e.aof.rewrites)
				require.NoError(t, e.CloseAOF())
				for i := 0; i < 2; i++ {
					e.resetStores()
					_, err := e.LoadAOF(path)
					require.NoError(t, err)
					switch mutation {
					case "replace":
						require.Equal(t, "replacement", runOn(t, e, "GET", "large"))
					case "delete":
						require.Equal(t, int64(0), runOn(t, e, "EXISTS", "large"))
					default:
						if kind == "set" {
							require.ElementsMatch(t, want, runOn(t, e, "SMEMBERS", "large"))
						} else if kind == "hash" {
							require.Equal(t, want, hashRewriteState(t, e, "large"))
						} else {
							require.Equal(t, want, runOn(t, e, "ZRANGE", "large", "0", "-1", "WITHSCORES"))
						}
						require.Greater(t, runOn(t, e, "PTTL", "large").(int64), int64(0))
					}
				}
			})
		}
	}
}

func TestCollectionRewriteHonorsByteBudgetAndOversizedMemberMakesProgress(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	for _, kind := range []string{"set", "zset", "list", "hash"} {
		t.Run(kind, func(t *testing.T) {
			e.resetStores()
			require.NoError(t, e.OpenAOF(filepath.Join(t.TempDir(), "log")))
			defer e.CloseAOF()
			for i := 0; i < 100; i++ {
				value := strconv.Itoa(i) + strings.Repeat("x", 4096)
				switch kind {
				case "hash":
					runOn(t, e, "HSET", "large", strconv.Itoa(i), value)
				case "set":
					runOn(t, e, "SADD", "large", value)
				case "list":
					runOn(t, e, "RPUSH", "large", value)
				case "zset":
					runOn(t, e, "ZADD", "large", strconv.Itoa(i), value)
				}
			}
			// One member exceeds the target. It must be emitted alone, never strand
			// the cursor or silently truncate the value.
			huge := strings.Repeat("y", 100000)
			switch kind {
			case "hash":
				runOn(t, e, "HSET", "large", "huge", huge)
			case "set":
				runOn(t, e, "SADD", "large", huge)
			case "list":
				runOn(t, e, "RPUSH", "large", huge)
			case "zset":
				runOn(t, e, "ZADD", "large", "-1", huge)
			}
			require.NoError(t, e.StartRewrite())
			chunks := 0
			for e.rewrite.active && chunks < 50 {
				before := e.rewrite.written
				require.NoError(t, e.AdvanceRewrite())
				waitForRewriteSyncOn(t, e)
				if e.rewrite.active {
					require.LessOrEqual(t, e.rewrite.written-before, int64(len(huge)+256))
				}
				chunks++
			}
			require.Greater(t, chunks, 5)
			require.False(t, e.rewrite.active)
			require.Equal(t, 1, e.aof.rewrites)
		})
	}
}

func hashRewriteState(t *testing.T, e *Engine, key string) map[string]string {
	t.Helper()
	h, ok := e.hashStore.Peek(key)
	require.True(t, ok)
	fields, values := h.Entries()
	out := make(map[string]string, len(fields))
	for i, field := range fields {
		out[field] = values[i]
	}
	return out
}
