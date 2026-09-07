package core

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRewriteStreamsOversizedRecordsAcrossMutation(t *testing.T) {
	for _, kind := range []string{"string", "hash", "list", "set", "zset"} {
		for _, mutation := range []string{"none", "replace", "delete"} {
			t.Run(kind+"/"+mutation, func(t *testing.T) {
				ResetStores()
				path := filepath.Join(t.TempDir(), "stream.aof")
				require.NoError(t, OpenAOF(path))
				t.Cleanup(func() { CancelRewrite(); require.NoError(t, CloseAOF()) })
				// Stream both the name and value; neither may be copied as a whole.
				key, value := strings.Repeat("k", 96<<10), strings.Repeat("v", 256<<10)
				switch kind {
				case "string":
					run(t, "SET", key, value)
				case "hash":
					run(t, "HSET", key, "field", value)
				case "list":
					run(t, "RPUSH", key, value)
				case "set":
					run(t, "SADD", key, value)
				case "zset":
					run(t, "ZADD", key, "1", value)
				}
				run(t, "PEXPIRE", key, "600000")
				expected := emitKey(nil, key)
				require.NoError(t, StartRewrite())
				require.NoError(t, AdvanceRewrite())
				require.NotNil(t, rewrite.stream, "a large record must yield before completion")
				require.LessOrEqual(t, rewrite.written, int64(rewriteRecordSlice))
				switch mutation {
				case "replace":
					run(t, "DEL", key)
					run(t, "SET", key, "replacement")
				case "delete":
					run(t, "DEL", key)
				}
				for cycles := 0; RewriteActive(); cycles++ {
					require.Less(t, cycles, 100)
					before := rewrite.written
					require.NoError(t, AdvanceRewrite())
					require.LessOrEqual(t, rewrite.written-before, int64(rewriteRecordSlice), "one-key records must be emitted in bounded slices")
				}
				require.NoError(t, CloseAOF())
				ResetStores()
				_, err := LoadAOF(path)
				require.NoError(t, err, "mutation must not leave a partial RESP record")
				switch mutation {
				case "none":
					require.Equal(t, expected, emitKey(nil, key))
				case "replace":
					require.Equal(t, "replacement", run(t, "GET", key))
					require.EqualValues(t, -1, run(t, "PTTL", key))
				case "delete":
					require.EqualValues(t, 0, run(t, "EXISTS", key))
				}
			})
		}
	}
}

func TestCancelPartialRewriteKeepsOriginalLog(t *testing.T) {
	ResetStores()
	path := filepath.Join(t.TempDir(), "cancel.aof")
	require.NoError(t, OpenAOF(path))
	t.Cleanup(func() { CancelRewrite(); require.NoError(t, CloseAOF()) })
	value := strings.Repeat("v", 4<<20)
	run(t, "SET", "large", value)
	require.NoError(t, StartRewrite())
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	require.NoError(t, AdvanceRewrite())
	runtime.ReadMemStats(&after)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(256<<10), "starting a large record must not allocate a whole encoded copy")
	require.NotNil(t, rewrite.stream)
	CancelRewrite()
	require.Nil(t, rewrite.stream)
	require.Empty(t, rewrite.collectionKey)
	require.NoError(t, CloseAOF())
	ResetStores()
	_, err := LoadAOF(path)
	require.NoError(t, err)
	require.Equal(t, value, run(t, "GET", "large"))
}

func TestRewriteDirtyBudgetRefusesBeforeRetainingName(t *testing.T) {
	ResetStores()
	path := filepath.Join(t.TempDir(), "dirty.aof")
	require.NoError(t, OpenAOF(path))
	t.Cleanup(func() { CancelRewrite(); require.NoError(t, CloseAOF()) })
	run(t, "SET", "original", "value")
	require.NoError(t, StartRewrite())
	before := rewriteBudgetAborts
	for i := 0; RewriteActive(); i++ {
		require.Less(t, i, 10)
		key := strconv.Itoa(i) + strings.Repeat("k", 1<<20)
		noteRewriteDirty(key)
		require.LessOrEqual(t, rewrite.dirtyBytes, rewriteDirtyBytes)
	}
	require.Equal(t, before+1, rewriteBudgetAborts)
	require.Nil(t, rewrite.dirty)
	require.Nil(t, rewrite.stream)
	_, err := os.Stat(path + ".rewrite")
	require.True(t, os.IsNotExist(err))
	run(t, "SET", "after-abort", "durable")
	require.NoError(t, CloseAOF())
	ResetStores()
	_, err = LoadAOF(path)
	require.NoError(t, err)
	require.Equal(t, "value", run(t, "GET", "original"))
	require.Equal(t, "durable", run(t, "GET", "after-abort"))
}
