package data_structure

import (
	"runtime"
	"strconv"
	"testing"
)

// This diagnostic uses only the public Hash methods so the same source can
// measure the baseline. Run with -benchtime=1x; ns/op includes construction/GC.
func BenchmarkHashPopulation(b *testing.B) {
	for _, members := range []int{0, 1, 8, 128} {
		b.Run("small-"+strconv.Itoa(members), func(b *testing.B) {
			const count = 10000
			for n := 0; n < b.N; n++ {
				before := heapBytes()
				population := make([]*Hash, count)
				for i := range population {
					h := NewHash()
					for member := 0; member < members; member++ {
						h.Set(strconv.Itoa(member), "value")
					}
					if h.Len() != members {
						b.Fatal("invalid population")
					}
					population[i] = h
				}
				after := heapBytes()
				b.ReportMetric(float64(int64(after)-int64(before))/count, "retained-B/collection")
				runtime.KeepAlive(population)
			}
		})
	}
	for _, grown := range []int{100000, 1000000} {
		b.Run("churn-"+strconv.Itoa(grown), func(b *testing.B) {
			for n := 0; n < b.N; n++ {
				before := heapBytes()
				h := NewHash()
				for member := 0; member < grown; member++ {
					h.Set(strconv.Itoa(member), "value")
				}
				if h.Len() != grown {
					b.Fatal("invalid high-water population")
				}
				peak := heapBytes()
				for member := 1000; member < grown; member++ {
					h.Del(strconv.Itoa(member))
				}
				if h.Len() != 1000 {
					b.Fatal("invalid surviving population")
				}
				after := heapBytes()
				b.ReportMetric(float64(int64(peak)-int64(before)), "grown-B")
				b.ReportMetric(float64(int64(after)-int64(before)), "shrunk-B")
				for member := 0; member < 1000; member++ {
					if value, ok := h.Get(strconv.Itoa(member)); !ok || value != "value" {
						b.Fatal("churn changed survivor")
					}
				}
				runtime.KeepAlive(h)
			}
		})
	}
}
