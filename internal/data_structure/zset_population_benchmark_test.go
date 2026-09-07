package data_structure

import (
	"runtime"
	"strconv"
	"testing"
)

// Run with -benchtime=1x. ns/op includes construction and GC so automatic
// calibration cannot multiply a large untimed fixture. Copying this diagnostic
// to a baseline needs no compaction-specific methods or fields.
func BenchmarkZSetSmallPopulation(b *testing.B) {
	for _, members := range []int{0, 1, 8, 128} {
		b.Run(strconv.Itoa(members), func(b *testing.B) {
			const count = 10000
			for n := 0; n < b.N; n++ {
				before := heapBytes()
				population := make([]*ZSet, count)
				for i := range population {
					z := CreateZSet()
					for member := 0; member < members; member++ {
						z.Add(float64(member), strconv.Itoa(member), 0)
					}
					if z.Len() != members {
						b.Fatal("invalid measurement population")
					}
					population[i] = z
				}
				after := heapBytes()
				b.ReportMetric(float64(int64(after)-int64(before))/count, "retained-B/collection")
				runtime.KeepAlive(population)
			}
		})
	}
}
