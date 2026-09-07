package data_structure

import (
	"bytes"
	"runtime"
	"strconv"
	"testing"
)

func TestSparseHLLEqualsDenseAcrossPromotionAndRestore(t *testing.T) {
	for _, size := range []int{0, 1, 50, 500, 513, 1000, 10000} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			compact := CreateHLL()
			dense := &HLL{regs: make([]byte, hllDenseSize)}
			for i := 0; i < size; i++ {
				item := "item:" + strconv.Itoa(i)
				if compact.Add(item) != dense.Add(item) {
					t.Fatal("encoding changed PFADD's changed result")
				}
			}
			if compact.Count() != dense.Count() || !bytes.Equal(compact.Marshal(), dense.Marshal()) {
				t.Fatal("compact representation changed count or legacy dump bytes")
			}
			payload := dense.Marshal()
			restored, err := UnmarshalHLL(payload)
			if err != nil || restored.Count() != dense.Count() || !bytes.Equal(restored.Marshal(), payload) {
				t.Fatal("legacy payload did not survive compact restore")
			}
			for i := range payload {
				payload[i] = 0
			}
			if !bytes.Equal(restored.Marshal(), dense.Marshal()) {
				t.Fatal("restore retained mutable input")
			}
			if size <= 500 && (compact.regs != nil || restored.regs != nil) {
				t.Fatal("small sketch unnecessarily retained dense registers")
			}
		})
	}
}

func TestSparseHLLMergesAtBothRepresentationBoundaries(t *testing.T) {
	for _, left := range []int{0, 100, 400, 2000} {
		for _, right := range []int{0, 100, 400, 2000} {
			a, b := CreateHLL(), CreateHLL()
			oracle := &HLL{regs: make([]byte, hllDenseSize)}
			for i := 0; i < left; i++ {
				item := "left:" + strconv.Itoa(i)
				a.Add(item)
				oracle.Add(item)
			}
			for i := 0; i < right; i++ {
				item := "right:" + strconv.Itoa(i)
				b.Add(item)
				oracle.Add(item)
			}
			bBefore := b.Marshal()
			a.Count() // Cache must become invalid after a changed merge.
			a.Merge(b)
			a.Merge(a) // Self-merge must not corrupt a sparse slice while iterating it.
			if !bytes.Equal(a.Marshal(), oracle.Marshal()) || a.Count() != oracle.Count() || !bytes.Equal(bBefore, b.Marshal()) {
				t.Fatalf("merge diverged or mutated source: %d, %d", left, right)
			}
		}
	}
}

func TestSparseHLLMemoryEstimateTracksHeap(t *testing.T) {
	for _, cardinality := range []int{0, 1, 128, 800} {
		t.Run(strconv.Itoa(cardinality), func(t *testing.T) {
			objects := make([]*HLL, 2000)
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			var estimate uint64
			for i := range objects {
				h := CreateHLL()
				for item := 0; item < cardinality; item++ {
					h.Add(strconv.Itoa(item))
				}
				objects[i] = h
				estimate += h.MemUsage()
			}
			runtime.GC()
			runtime.ReadMemStats(&after)
			runtime.KeepAlive(objects)
			actual := int64(after.HeapAlloc) - int64(before.HeapAlloc)
			ratio := float64(estimate) / float64(actual)
			t.Logf("items=%d estimated=%d actual=%d ratio=%.3f", cardinality, estimate, actual, ratio)
			if actual <= 0 || ratio < .85 || ratio > 1.15 {
				t.Fatal("memory accounting differs from live heap")
			}
		})
	}
}

func FuzzSparseHLLMatchesDense(f *testing.F) {
	f.Add([]byte("small\x00\xffsketch"))
	f.Add(bytes.Repeat([]byte("large-state-promotion"), 100))
	f.Fuzz(func(t *testing.T, input []byte) {
		if len(input) > 4096 {
			t.Skip()
		}
		compact := CreateHLL()
		dense := &HLL{regs: make([]byte, hllDenseSize)}
		for i, value := range input {
			item := strconv.Itoa(i) + ":" + string([]byte{value})
			if compact.Add(item) != dense.Add(item) {
				t.Fatal("PFADD changed flag mismatch")
			}
			if i%128 == 0 && compact.Count() != dense.Count() {
				t.Fatal("cached count mismatch")
			}
		}
		if compact.Count() != dense.Count() || !bytes.Equal(compact.Marshal(), dense.Marshal()) {
			t.Fatal("register mismatch")
		}
	})
}
