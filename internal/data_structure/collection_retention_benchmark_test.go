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
func BenchmarkCollectionMapRetention(b *testing.B) {
	for _, survivors := range []int{1, 1000} {
		for _, kind := range []string{"hash", "zset"} {
			b.Run(kind+"/survivors-"+strconv.Itoa(survivors), func(b *testing.B) {
				const grown = 100000
				for iteration := 0; iteration < b.N; iteration++ {
					b.StopTimer()
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
					peak := retainedProfileHeap()
					for i := survivors; i < grown; i++ {
						if h != nil {
							h.Del(strconv.Itoa(i))
						} else {
							z.Remove(strconv.Itoa(i))
						}
					}
					shrunk := retainedProfileHeap()
					b.StartTimer()
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
					b.StopTimer()
					after := retainedProfileHeap()
					b.ReportMetric(float64(int64(peak)-int64(before)), "grown-B")
					b.ReportMetric(float64(int64(shrunk)-int64(before)), "shrunk-B")
					b.ReportMetric(float64(int64(after)-int64(before)), "rebuilt-B")
					b.ReportMetric(float64(int64(shrunk)-int64(after)), "recovered-B")
					b.Logf("%s survivors=%d grown=%d shrunk=%d rebuilt=%d recovered=%d synchronous-probe=%v hash-struct=%d zset-struct=%d",
						kind, survivors, int64(peak)-int64(before), int64(shrunk)-int64(before), int64(after)-int64(before), int64(shrunk)-int64(after), rebuild, unsafe.Sizeof(Hash{}), unsafe.Sizeof(ZSet{}))
					runtime.KeepAlive(h)
					runtime.KeepAlive(z)
				}
			})
		}
	}
}

func retainedProfileHeap() uint64 {
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats.HeapAlloc
}
