package core

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRewriteStreamsOversizedRecordsAcrossMutation(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	for _, kind := range []string{"string", "hash", "list", "set", "zset"} {
		for _, mutation := range []string{"none", "replace", "delete", "delete/slow-sync"} {
			t.Run(kind+"/"+mutation, func(t *testing.T) {
				e.resetStores()
				path := filepath.Join(t.TempDir(), "stream.aof")
				require.NoError(t, e.OpenAOF(path))
				t.Cleanup(func() { e.CancelRewrite(); require.NoError(t, e.CloseAOF()) })
				// A slow disk: the log's own fsync is still running when the
				// rewrite is ready to finish, as on a cold macOS runner.
				slowSync := mutation == "delete/slow-sync"
				if slowSync {
					mutation = "delete"
					oldSync := e.aofSync
					e.aofSync = func(f *os.File) error { time.Sleep(300 * time.Millisecond); return f.Sync() }
					t.Cleanup(func() { e.aofSync = oldSync })
				}
				// Stream both the name and value; neither may be copied as a whole.
				key, value := strings.Repeat("k", 96<<10), strings.Repeat("v", 256<<10)
				switch kind {
				case "string":
					runOn(t, e, "SET", key, value)
				case "hash":
					runOn(t, e, "HSET", key, "field", value)
				case "list":
					runOn(t, e, "RPUSH", key, value)
				case "set":
					runOn(t, e, "SADD", key, value)
				case "zset":
					runOn(t, e, "ZADD", key, "1", value)
				}
				runOn(t, e, "PEXPIRE", key, "600000")
				expected := e.emitKey(nil, key)
				require.NoError(t, e.StartRewrite())
				require.NoError(t, e.AdvanceRewrite())
				waitForRewriteSyncOn(t, e)
				require.NotNil(t, e.rewrite.stream, "a large record must yield before completion")
				require.LessOrEqual(t, e.rewrite.written, int64(rewriteRecordSlice))
				switch mutation {
				case "replace":
					runOn(t, e, "DEL", key)
					runOn(t, e, "SET", key, "replacement")
				case "delete":
					runOn(t, e, "DEL", key)
				}
				if slowSync {
					// Write the DEL and start its everysec sync now.
					e.aof.lastSync = time.Time{}
					require.NoError(t, e.flushAOF(false))
					require.NotNil(t, e.aof.syncPending, "the slow sync must be running while the rewrite advances")
				}
				for cycles := 0; e.RewriteActive(); cycles++ {
					require.Less(t, cycles, 100)
					// The log's own append or sync may be pending, as after
					// the DEL above.
					waitForLogOn(e)
					before := e.rewrite.written
					require.NoError(t, e.AdvanceRewrite())
					waitForRewriteSyncOn(t, e)
					require.LessOrEqual(t, e.rewrite.written-before, int64(rewriteRecordSlice), "one-key records must be emitted in bounded slices")
				}
				require.NoError(t, e.CloseAOF())
				e.resetStores()
				_, err := e.LoadAOF(path)
				require.NoError(t, err, "mutation must not leave a partial RESP record")
				switch mutation {
				case "none":
					require.Equal(t, expected, e.emitKey(nil, key))
				case "replace":
					require.Equal(t, "replacement", runOn(t, e, "GET", key))
					require.EqualValues(t, -1, runOn(t, e, "PTTL", key))
				case "delete":
					require.EqualValues(t, 0, runOn(t, e, "EXISTS", key))
				}
			})
		}
	}
}

func TestCancelPartialRewriteKeepsOriginalLog(t *testing.T) {
	// Not parallel: it reads the process's heap statistics, which a test
	// running beside it would move.
	e := newTestEngine(t, Options{})
	path := filepath.Join(t.TempDir(), "cancel.aof")
	require.NoError(t, e.OpenAOF(path))
	t.Cleanup(func() { e.CancelRewrite(); require.NoError(t, e.CloseAOF()) })
	value := strings.Repeat("v", 4<<20)
	runOn(t, e, "SET", "large", value)
	require.NoError(t, e.StartRewrite())
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	require.NoError(t, e.AdvanceRewrite())
	waitForRewriteSyncOn(t, e)
	runtime.ReadMemStats(&after)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(256<<10), "starting a large record must not allocate a whole encoded copy")
	require.NotNil(t, e.rewrite.stream)
	e.CancelRewrite()
	require.Nil(t, e.rewrite.stream)
	require.Empty(t, e.rewrite.collectionKey)
	require.NoError(t, e.CloseAOF())
	e.resetStores()
	_, err := e.LoadAOF(path)
	require.NoError(t, err)
	require.Equal(t, value, runOn(t, e, "GET", "large"))
}

func TestRewriteDirtyBudgetRefusesBeforeRetainingName(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := filepath.Join(t.TempDir(), "dirty.aof")
	require.NoError(t, e.OpenAOF(path))
	t.Cleanup(func() { e.CancelRewrite(); require.NoError(t, e.CloseAOF()) })
	runOn(t, e, "SET", "original", "value")
	require.NoError(t, e.StartRewrite())
	before := e.rewriteBudgetAborts
	for i := 0; e.RewriteActive(); i++ {
		require.Less(t, i, 10)
		key := strconv.Itoa(i) + strings.Repeat("k", 1<<20)
		e.noteRewriteDirty(key)
		require.LessOrEqual(t, e.rewrite.dirtyBytes, rewriteDirtyBytes)
	}
	require.Equal(t, before+1, e.rewriteBudgetAborts)
	require.Nil(t, e.rewrite.dirty)
	require.Nil(t, e.rewrite.stream)
	_, err := os.Stat(path + ".rewrite")
	require.True(t, os.IsNotExist(err))
	runOn(t, e, "SET", "after-abort", "durable")
	require.NoError(t, e.CloseAOF())
	e.resetStores()
	_, err = e.LoadAOF(path)
	require.NoError(t, err)
	require.Equal(t, "value", runOn(t, e, "GET", "original"))
	require.Equal(t, "durable", runOn(t, e, "GET", "after-abort"))
}
