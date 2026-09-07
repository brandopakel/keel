package core

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/brandopakel/keel/internal/data_structure"
	"github.com/stretchr/testify/require"
)

func TestSketchDumpSlicesPreserveEnvelopeAcrossAllBoundaries(t *testing.T) {
	for _, kind := range []string{"cms", "morris"} {
		for _, slice := range []int{1, 2, 3, 4, 5, 7, 16, 24, 64, 65536} {
			ResetStores()
			if kind == "cms" {
				cms := data_structure.CreateCMS(129, 3)
				cms.IncrBy("item", 123456)
				cmsStore.Put("image", cms)
			} else {
				morris := data_structure.CreateMorris(129, 3)
				morris.IncrBy("item", 123456)
				morrisStore.Put("image", morris)
			}
			want, _ := dumpKey("image")
			stream := newSketchDumpStream("image")
			var got []byte
			for offset := 0; offset < stream.size(); offset += slice {
				got = stream.appendSlice(got, offset, min(slice, stream.size()-offset))
			}
			require.Equal(t, want, got, "%s slice=%d", kind, slice)
			require.NoError(t, restoreKey("restored", got))
		}
	}
	ResetStores()
}

func TestSketchRewriteReconcilesWritesAcrossBodySlices(t *testing.T) {
	for _, kind := range []string{"cms", "morris"} {
		t.Run(kind, func(t *testing.T) {
			ResetStores()
			path := filepath.Join(t.TempDir(), "store.aof")
			require.NoError(t, OpenAOF(path))
			t.Cleanup(func() { CancelRewrite(); require.NoError(t, CloseAOF()); ResetStores() })
			command := "CMS.INCRBY"
			if kind == "cms" {
				cmsStore.Put("image", data_structure.CreateCMS(1<<20, 1))
			} else {
				morrisStore.Put("image", data_structure.CreateMorris(4<<20, 1))
				command = "MORRIS.INCRBY"
			}
			run(t, "PEXPIRE", "image", "600000")
			require.NoError(t, StartRewrite())
			require.NoError(t, AdvanceRewrite())
			require.NotNil(t, rewrite.stream.sketch)
			// Change cells after the header and first counter bytes were emitted.
			// Every intermediate record must still decode, then reconciliation
			// must replace it with the exact final table and RNG state.
			for i := 0; i < 32; i++ {
				run(t, command, "image", fmt.Sprintf("item:%d", i), "12345")
				require.NoError(t, AdvanceRewrite())
			}
			want, _ := dumpKey("image")
			for cycles := 0; RewriteActive(); cycles++ {
				require.Less(t, cycles, 300)
				require.NoError(t, FlushAOF())
				waitForRewriteSync(t)
			}
			require.NoError(t, CloseAOF())
			for restart := 0; restart < 2; restart++ {
				ResetStores()
				_, err := LoadAOF(path)
				require.NoError(t, err)
				got, present := dumpKey("image")
				require.True(t, present)
				require.Equal(t, want, got)
				require.Positive(t, run(t, "PTTL", "image").(int64))
			}
		})
	}
}
