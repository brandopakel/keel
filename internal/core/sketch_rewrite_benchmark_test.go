package core

import (
	"fmt"
	"testing"

	"github.com/brandopakel/keel/internal/data_structure"
)

func BenchmarkSketchRewriteStart(b *testing.B) {
	for _, mib := range []int{4, 64} {
		for _, kind := range []string{"cms", "morris"} {
			b.Run(fmt.Sprintf("%s/%dMiB", kind, mib), func(b *testing.B) {
				ResetStores()
				if kind == "cms" {
					cmsStore.Put("image", data_structure.CreateCMS(uint32(mib<<18), 1))
				} else {
					morrisStore.Put("image", data_structure.CreateMorris(uint32(mib<<20), 1))
				}
				b.Cleanup(func() { rewrite.stream = nil; ResetStores() })
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					out := emitRewriteKey(nil, "image", false)
					if len(out) != rewriteRecordSlice {
						b.Fatal("first slice must fill its budget")
					}
					rewrite.stream = nil
				}
			})
		}
	}
}
