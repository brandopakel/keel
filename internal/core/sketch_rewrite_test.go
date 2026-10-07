package core

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/brandopakel/keel/internal/data_structure"
	"github.com/stretchr/testify/require"
)

func TestSketchDumpSlicesPreserveEnvelopeAcrossAllBoundaries(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	for _, kind := range []string{"cms", "morris"} {
		for _, slice := range []int{1, 2, 3, 4, 5, 7, 16, 24, 64, 65536} {
			e.resetStores()
			if kind == "cms" {
				cms := data_structure.CreateCMS(129, 3)
				cms.IncrBy("item", 123456)
				e.cmsStore.Put("image", cms)
			} else {
				morris := data_structure.CreateMorris(129, 3)
				morris.IncrBy("item", 123456)
				e.morrisStore.Put("image", morris)
			}
			want, _ := e.dumpKey("image")
			stream := e.newSketchDumpStream("image")
			var got []byte
			for offset := 0; offset < stream.size(); offset += slice {
				got = stream.appendSlice(got, offset, min(slice, stream.size()-offset))
			}
			require.Equal(t, want, got, "%s slice=%d", kind, slice)
			require.NoError(t, e.restoreKey("restored", got))
		}
	}
}

func TestSketchRewriteReconcilesWritesAcrossBodySlices(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	for _, kind := range []string{"cms", "morris", "cms/slow-sync"} {
		t.Run(kind, func(t *testing.T) {
			e.resetStores()
			path := filepath.Join(t.TempDir(), "store.aof")
			require.NoError(t, e.OpenAOF(path))
			t.Cleanup(func() { e.CancelRewrite(); require.NoError(t, e.CloseAOF()); e.resetStores() })
			// A slow disk: the log's own fsync is still running when the
			// rewrite is ready to finish, as on a busy CI runner.
			slowSync := kind == "cms/slow-sync"
			if slowSync {
				kind = "cms"
				oldSync := e.aofSync
				e.aofSync = func(f *os.File) error { time.Sleep(300 * time.Millisecond); return f.Sync() }
				t.Cleanup(func() { e.aofSync = oldSync })
			}
			command := "CMS.INCRBY"
			if kind == "cms" {
				e.cmsStore.Put("image", data_structure.CreateCMS(1<<20, 1))
			} else {
				e.morrisStore.Put("image", data_structure.CreateMorris(4<<20, 1))
				command = "MORRIS.INCRBY"
			}
			runOn(t, e, "PEXPIRE", "image", "600000")
			require.NoError(t, e.StartRewrite())
			require.NoError(t, e.AdvanceRewrite())
			require.NotNil(t, e.rewrite.stream.sketch)
			// Change cells after the header and first counter bytes were emitted.
			// Every intermediate record must still decode, then reconciliation
			// must replace it with the exact final table and RNG state.
			for i := 0; i < 32; i++ {
				runOn(t, e, command, "image", fmt.Sprintf("item:%d", i), "12345")
				require.NoError(t, e.AdvanceRewrite())
			}
			want, _ := e.dumpKey("image")
			if slowSync {
				// Write the increments and start their everysec sync now.
				e.aof.lastSync = time.Time{}
				require.NoError(t, e.flushAOF(false))
				require.NotNil(t, e.aof.syncPending, "the slow sync must be running while the rewrite advances")
			}
			for cycles := 0; e.RewriteActive(); cycles++ {
				require.Less(t, cycles, 300)
				waitForLogOn(e)
				require.NoError(t, e.FlushAOF())
				waitForRewriteSyncOn(t, e)
			}
			require.NoError(t, e.CloseAOF())
			for restart := 0; restart < 2; restart++ {
				e.resetStores()
				_, err := e.LoadAOF(path)
				require.NoError(t, err)
				got, present := e.dumpKey("image")
				require.True(t, present)
				require.Equal(t, want, got)
				require.Positive(t, runOn(t, e, "PTTL", "image").(int64))
			}
		})
	}
}
