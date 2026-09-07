package data_structure

import (
	"runtime"
	"strconv"
	"testing"
	"time"
)

func BenchmarkZSetChurnCompaction(b *testing.B) {
	for iteration := 0; iteration < b.N; iteration++ {
		z := CreateZSet()
		for member := 0; member < 1000000; member++ {
			z.Add(float64(member), strconv.Itoa(member), 0)
		}
		if z.Len() != 1000000 {
			b.Fatal("invalid high-water population")
		}
		for member := 1000; member < 1000000; member++ {
			z.Remove(strconv.Itoa(member))
		}
		if z.Len() != 1000 || z.indexState == nil || z.indexState.next == nil {
			b.Fatal("fixture has no pending compaction")
		}
		before := heapBytes()
		work, slices := 0, 0
		var worst time.Duration
		for z.indexState != nil && slices < 100 {
			start := time.Now()
			used := z.CompactIndex(37)
			worst = max(worst, time.Since(start))
			if used > 37 {
				b.Fatal("compaction exceeded work budget")
			}
			work += used
			slices++
		}
		if z.indexState != nil || work != 1000 {
			b.Fatal("compaction did not finish exactly the surviving population")
		}
		after := heapBytes()
		for member := 0; member < 1000; member++ {
			if score, ok := z.Score(strconv.Itoa(member)); !ok || score != float64(member) {
				b.Fatal("compaction changed survivor")
			}
		}
		b.ReportMetric(float64(int64(before)-int64(after)), "recovered-B")
		b.ReportMetric(float64(worst.Nanoseconds()), "worst-slice-ns")
		b.ReportMetric(float64(slices), "slices")
		runtime.KeepAlive(z)
	}
}
