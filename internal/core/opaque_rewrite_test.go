package core

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/brandopakel/keel/internal/data_structure"
	"github.com/stretchr/testify/require"
)

func TestOpaqueRewriteKeepsOnePayloadAndEmitsBoundedRecord(t *testing.T) {
	ResetStores()
	require.NoError(t, OpenAOF(filepath.Join(t.TempDir(), "store.aof")))
	t.Cleanup(func() { CancelRewrite(); require.NoError(t, CloseAOF()); ResetStores() })
	cms := data_structure.CreateCMS(1<<20, 1)
	cmsStore.Put("large", cms)
	require.NoError(t, StartRewrite())
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	require.NoError(t, AdvanceRewrite())
	runtime.ReadMemStats(&after)
	t.Logf("payload=%d allocated=%d first_slice=%d", cms.MarshalSize()+9, after.TotalAlloc-before.TotalAlloc, rewrite.written)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(cms.MarshalSize()+(256<<10)))
	require.LessOrEqual(t, rewrite.written, int64(rewriteRecordSlice))
	require.NotNil(t, rewrite.stream)
}

func TestOpaqueRewriteReconcilesMutationWithValidHistoricalPayload(t *testing.T) {
	for _, kind := range []string{"bloom", "cms", "morris", "hll", "cuckoo"} {
		for _, mutation := range []string{"none", "update", "replace", "delete"} {
			t.Run(kind+"/"+mutation, func(t *testing.T) {
				ResetStores()
				path := filepath.Join(t.TempDir(), "store.aof")
				require.NoError(t, OpenAOF(path))
				t.Cleanup(func() { CancelRewrite(); require.NoError(t, CloseAOF()); ResetStores() })
				key := strings.Repeat("k", 96<<10)
				var update []string
				switch kind {
				case "bloom":
					run(t, "BF.RESERVE", key, "0.01", "100000")
					update = []string{"BF.ADD", key, "new"}
				case "cms":
					run(t, "CMS.INITBYDIM", key, "65536", "1")
					update = []string{"CMS.INCRBY", key, "new", "5"}
				case "morris":
					run(t, "MORRIS.INITBYDIM", key, "65536", "1")
					update = []string{"MORRIS.INCRBY", key, "new", "5"}
				case "hll":
					run(t, "PFADD", key, "old")
					update = []string{"PFADD", key, "new"}
				case "cuckoo":
					run(t, "CF.RESERVE", key, "65536")
					update = []string{"CF.ADD", key, "new"}
				}
				run(t, "PEXPIRE", key, "600000")
				require.NoError(t, FlushAOF())
				require.NoError(t, StartRewrite())
				require.NoError(t, AdvanceRewrite())
				require.NotNil(t, rewrite.stream)
				require.LessOrEqual(t, rewrite.written, int64(rewriteRecordSlice))
				switch mutation {
				case "update":
					run(t, update[0], update[1:]...)
				case "replace":
					run(t, "DEL", key)
					run(t, "SET", key, "replacement")
				case "delete":
					run(t, "DEL", key)
				}
				want, present := dumpKey(key)
				for cycles := 0; RewriteActive(); cycles++ {
					waitForRewriteSync(t)
					require.Less(t, cycles, 100)
					before := rewrite.written
					require.NoError(t, FlushAOF())
					require.LessOrEqual(t, rewrite.written-before, int64(rewriteRecordSlice))
				}
				require.NoError(t, CloseAOF())
				for restart := 0; restart < 2; restart++ {
					ResetStores()
					_, err := LoadAOF(path)
					require.NoError(t, err, "every intermediate RESTORE must remain checksum-valid")
					got, exists := dumpKey(key)
					require.Equal(t, present, exists)
					require.Equal(t, want, got)
					if mutation == "none" || mutation == "update" {
						require.Positive(t, run(t, "PTTL", key).(int64))
					}
				}
			})
		}
	}
}
