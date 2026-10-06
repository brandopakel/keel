package core

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/brandopakel/keel/internal/data_structure"
	"github.com/stretchr/testify/require"
)

func TestSketchRewriteStartsWithoutConstructingAWholePayload(t *testing.T) {
	// Not parallel: it reads the process's heap statistics, which a test
	// running beside it would move.
	e := newTestEngine(t, Options{})
	require.NoError(t, e.OpenAOF(filepath.Join(t.TempDir(), "store.aof")))
	t.Cleanup(func() { e.CancelRewrite(); require.NoError(t, e.CloseAOF()); e.resetStores() })
	cms := data_structure.CreateCMS(1<<20, 1)
	e.cmsStore.Put("large", cms)
	require.NoError(t, e.StartRewrite())
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	require.NoError(t, e.AdvanceRewrite())
	runtime.ReadMemStats(&after)
	t.Logf("payload=%d allocated=%d first_slice=%d", cms.MarshalSize()+9, after.TotalAlloc-before.TotalAlloc, e.rewrite.written)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(256<<10))
	require.LessOrEqual(t, e.rewrite.written, int64(rewriteRecordSlice))
	require.NotNil(t, e.rewrite.stream)
	require.Empty(t, e.rewrite.stream.payload)
	require.NotNil(t, e.rewrite.stream.sketch)
}

func TestOpaqueRewriteReconcilesMutationWithValidHistoricalPayload(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	for _, kind := range []string{"bloom", "cms", "morris", "hll", "cuckoo"} {
		for _, mutation := range []string{"none", "update", "replace", "delete"} {
			t.Run(kind+"/"+mutation, func(t *testing.T) {
				e.resetStores()
				path := filepath.Join(t.TempDir(), "store.aof")
				require.NoError(t, e.OpenAOF(path))
				t.Cleanup(func() { e.CancelRewrite(); require.NoError(t, e.CloseAOF()); e.resetStores() })
				key := strings.Repeat("k", 96<<10)
				var update []string
				switch kind {
				case "bloom":
					runOn(t, e, "BF.RESERVE", key, "0.01", "100000")
					update = []string{"BF.ADD", key, "new"}
				case "cms":
					runOn(t, e, "CMS.INITBYDIM", key, "65536", "1")
					update = []string{"CMS.INCRBY", key, "new", "5"}
				case "morris":
					runOn(t, e, "MORRIS.INITBYDIM", key, "65536", "1")
					update = []string{"MORRIS.INCRBY", key, "new", "5"}
				case "hll":
					runOn(t, e, "PFADD", key, "old")
					update = []string{"PFADD", key, "new"}
				case "cuckoo":
					runOn(t, e, "CF.RESERVE", key, "65536")
					update = []string{"CF.ADD", key, "new"}
				}
				runOn(t, e, "PEXPIRE", key, "600000")
				require.NoError(t, e.FlushAOF())
				require.NoError(t, e.StartRewrite())
				require.NoError(t, e.AdvanceRewrite())
				require.NotNil(t, e.rewrite.stream)
				require.LessOrEqual(t, e.rewrite.written, int64(rewriteRecordSlice))
				switch mutation {
				case "update":
					runOn(t, e, update[0], update[1:]...)
				case "replace":
					runOn(t, e, "DEL", key)
					runOn(t, e, "SET", key, "replacement")
				case "delete":
					runOn(t, e, "DEL", key)
				}
				want, present := e.dumpKey(key)
				for cycles := 0; e.RewriteActive(); cycles++ {
					waitForRewriteSyncOn(t, e)
					require.Less(t, cycles, 100)
					before := e.rewrite.written
					require.NoError(t, e.FlushAOF())
					require.LessOrEqual(t, e.rewrite.written-before, int64(rewriteRecordSlice))
				}
				require.NoError(t, e.CloseAOF())
				for restart := 0; restart < 2; restart++ {
					e.resetStores()
					_, err := e.LoadAOF(path)
					require.NoError(t, err, "every intermediate RESTORE must remain checksum-valid")
					got, exists := e.dumpKey(key)
					require.Equal(t, present, exists)
					require.Equal(t, want, got)
					if mutation == "none" || mutation == "update" {
						require.Positive(t, runOn(t, e, "PTTL", key).(int64))
					}
				}
			})
		}
	}
}
