package data_structure

import (
	"runtime"
	"strconv"
	"testing"
	"time"
	"unsafe"
)

// This opt-in diagnostic isolates map capacity retained after collection churn.
// Replacing the map here is a measurement probe, not a serving implementation:
// it walks synchronously and has no mutation, scheduling or admission contract.
// ns/op includes the complete grow/shrink/probe cycle to keep automatic benchmark
// iteration calibration proportional to the actual work. The logged probe time
// measures map replacement separately. Use -benchtime=1x for heap samples.
func BenchmarkCollectionMapRetention(b *testing.B) {
	for _, grown := range []int{100000, 1000000} {
		for _, survivors := range []int{1, 1000} {
			for _, kind := range []string{"hash", "zset"} {
				b.Run(kind+"/grown-"+strconv.Itoa(grown)+"/survivors-"+strconv.Itoa(survivors), func(b *testing.B) {
					for iteration := 0; iteration < b.N; iteration++ {
						before := retainedProfileHeap()
						var h *Hash
						var z *ZSet
						if kind == "hash" {
							h = NewHash()
							for i := 0; i < grown; i++ {
								h.Set(strconv.Itoa(i), "value")
							}
						} else {
							z = CreateZSet()
							for i := 0; i < grown; i++ {
								z.Add(float64(i), strconv.Itoa(i), 0)
							}
						}
						if h != nil && h.Len() != grown {
							b.Fatalf("hash grew to %d, want %d", h.Len(), grown)
						}
						if z != nil && z.Len() != grown {
							b.Fatalf("sorted set grew to %d, want %d", z.Len(), grown)
						}
						peak := retainedProfileHeap()
						for i := survivors; i < grown; i++ {
							if h != nil {
								h.Del(strconv.Itoa(i))
							} else {
								z.Remove(strconv.Itoa(i))
							}
						}
						if h != nil && h.Len() != survivors {
							b.Fatalf("hash survivors=%d, want %d", h.Len(), survivors)
						}
						if z != nil && z.Len() != survivors {
							b.Fatalf("sorted-set survivors=%d, want %d", z.Len(), survivors)
						}
						shrunk := retainedProfileHeap()
						started := time.Now()
						if h != nil {
							next := make(map[string]string, h.Len())
							for key, value := range h.fields {
								next[key] = value
							}
							h.fields = next
						} else {
							next := make(map[string]float64, z.Len())
							for key, value := range z.dict {
								next[key] = value
							}
							z.dict = next
						}
						rebuild := time.Since(started)
						after := retainedProfileHeap()
						b.ReportMetric(float64(int64(peak)-int64(before)), "grown-B")
						b.ReportMetric(float64(int64(shrunk)-int64(before)), "shrunk-B")
						b.ReportMetric(float64(int64(after)-int64(before)), "rebuilt-B")
						b.ReportMetric(float64(int64(shrunk)-int64(after)), "recovered-B")
						b.Logf("%s survivors=%d grown=%d shrunk=%d rebuilt=%d recovered=%d synchronous-probe=%v hash-struct=%d zset-struct=%d",
							kind, survivors, int64(peak)-int64(before), int64(shrunk)-int64(before), int64(after)-int64(before), int64(shrunk)-int64(after), rebuild, unsafe.Sizeof(Hash{}), unsafe.Sizeof(ZSet{}))
						for member := 0; member < survivors; member++ {
							key := strconv.Itoa(member)
							if h != nil {
								if value, ok := h.Get(key); !ok || value != "value" {
									b.Fatal("rebuild changed hash survivor", key)
								}
							}
							if z != nil {
								if score, ok := z.Score(key); !ok || score != float64(member) {
									b.Fatal("rebuild changed sorted-set survivor", key)
								}
							}
						}
						runtime.KeepAlive(h)
						runtime.KeepAlive(z)
					}
				})
			}
		}
	}
}

func retainedProfileHeap() uint64 {
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats.HeapAlloc
}
