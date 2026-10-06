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
				e := newEngine(Options{})
				if kind == "cms" {
					e.cmsStore.Put("image", data_structure.CreateCMS(uint32(mib<<18), 1))
				} else {
					e.morrisStore.Put("image", data_structure.CreateMorris(uint32(mib<<20), 1))
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					out := e.emitRewriteKey(nil, "image", false)
					if len(out) != rewriteRecordSlice {
						b.Fatal("first slice must fill its budget")
					}
					e.rewrite.stream = nil
				}
			})
		}
	}
}
