package data_structure

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"memkv/internal/config"
)

func heapBytes() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// TestEstimateTracksRealHeap is the test that keeps the accounting honest.
//
// entryOverhead is a measured constant, not a derived one, so it can drift out
// of date - a field added to Obj, a change in how values are stored, a new Go
// map implementation. Comparing the estimate against HeapAlloc catches that,
// which reading the struct definitions would not.
//
// The estimate is expected to sit below reality, because it ignores the
// allocator rounding each value up to a size class. The bound below allows for
// that while still failing if the accounting stops describing the same thing.
func TestEstimateTracksRealHeap(t *testing.T) {
	withEviction(t, config.EvictFirst, 5, 100000000)

	for _, valLen := range []int{8, 64, 512, 4096} {
		d := CreateDict()

		before := heapBytes()
		const n = 100000
		for i := 0; i < n; i++ {
			// Each value must be its own allocation. Storing one shared string
			// would measure a single value against 100,000 entries, because Go
			// strings are immutable and share backing storage - which the real
			// server never does, since every value is a fresh string built by
			// the parser from the wire.
			d.Put("key:"+strconv.Itoa(i), d.NewObj(fmt.Sprintf("%0*d", valLen, i), 0, 0, 0))
		}
		actual := heapBytes() - before
		runtime.KeepAlive(d)

		estimated := d.MemUsed()
		ratio := float64(estimated) / float64(actual)
		t.Logf("valLen=%-5d estimated=%-10d actual=%-10d ratio=%.3f", valLen, estimated, actual, ratio)

		// Measured at 1.002-1.061. The bounds allow drift without allowing the
		// accounting to stop describing the same thing.
		assert.Greater(t, ratio, 0.90,
			"valLen=%d: estimate is %.0f%% of real heap, too far below to bound anything", valLen, ratio*100)
		assert.Less(t, ratio, 1.15,
			"valLen=%d: estimate is %.0f%% of real heap, so it over-counts badly", valLen, ratio*100)
	}
}

func TestMemUsedRisesAndFallsWithTheKeyspace(t *testing.T) {
	withEviction(t, config.EvictFirst, 5, 1000000)
	d := CreateDict()
	assert.Equal(t, uint64(0), d.MemUsed(), "an empty dictionary holds nothing")

	d.Put("k", d.NewObj(strings.Repeat("v", 1000), 0, 0, 0))
	withValue := d.MemUsed()
	assert.Greater(t, withValue, uint64(1000), "the value's bytes must be counted")

	d.Del("k")
	assert.Equal(t, uint64(0), d.MemUsed(), "deleting must return exactly what it charged")
}

// TestOverwritingReplacesCostRatherThanAddingIt is the accounting mistake that
// would otherwise leak: charge for the new value without refunding the old, and
// the estimate climbs forever on a key that is merely being updated.
func TestOverwritingReplacesCostRatherThanAddingIt(t *testing.T) {
	withEviction(t, config.EvictFirst, 5, 1000000)
	d := CreateDict()

	d.Put("k", d.NewObj(strings.Repeat("v", 1000), 0, 0, 0))
	first := d.MemUsed()
	for i := 0; i < 100; i++ {
		d.Put("k", d.NewObj(strings.Repeat("v", 1000), 0, 0, 0))
	}
	assert.Equal(t, first, d.MemUsed(), "rewriting one key must not accumulate cost")

	d.Put("k", d.NewObj(strings.Repeat("v", 5000), 0, 0, 0))
	assert.Greater(t, d.MemUsed(), first, "a larger value must cost more")
	d.Put("k", d.NewObj("v", 0, 0, 0))
	assert.Less(t, d.MemUsed(), first, "a smaller value must cost less")
}

// TestMemoryBoundHoldsRegardlessOfValueSize is the point of the whole change: a
// key limit cannot tell an 8-byte value from an 8KB one, and a memory limit
// must.
func TestMemoryBoundHoldsRegardlessOfValueSize(t *testing.T) {
	for _, valLen := range []int{8, 800, 8000} {
		withEviction(t, config.LRU, 5, 100000000)
		config.MaxMemory = 1 << 20 // 1 MB
		t.Cleanup(func() { config.MaxMemory = 0 })

		d := CreateDict()
		val := strings.Repeat("v", valLen)
		for i := 0; i < 20000; i++ {
			d.Put("key:"+strconv.Itoa(i), d.NewObj(val, 0, 0, 0))
			assert.LessOrEqual(t, d.MemUsed(), uint64(1<<20)+uint64(valLen)+entryOverhead+16,
				"valLen=%d: the dictionary must stay within its memory bound", valLen)
		}
		assert.Greater(t, d.Evicted(), uint64(0), "valLen=%d: eviction should have run", valLen)
		t.Logf("valLen=%-5d held %d keys in 1MB", valLen, d.Len())
	}
}

// TestKeyCountAdaptsToValueSize states the difference from -maxkeys directly:
// under a byte budget, big values mean fewer keys.
func TestKeyCountAdaptsToValueSize(t *testing.T) {
	held := map[int]int{}
	for _, valLen := range []int{8, 800, 8000} {
		withEviction(t, config.LRU, 5, 100000000)
		config.MaxMemory = 1 << 20
		t.Cleanup(func() { config.MaxMemory = 0 })

		d := CreateDict()
		val := strings.Repeat("v", valLen)
		for i := 0; i < 20000; i++ {
			d.Put("key:"+strconv.Itoa(i), d.NewObj(val, 0, 0, 0))
		}
		held[valLen] = d.Len()
	}
	fmt.Printf("  keys held in a 1MB budget: 8B=%d  800B=%d  8000B=%d\n",
		held[8], held[800], held[8000])
	assert.Greater(t, held[8], held[800]*5, "small values should fit far more keys")
	assert.Greater(t, held[800], held[8000]*5)
}

// TestAValueLargerThanTheBudgetDoesNotEmptyTheKeyspaceForever guards the loop
// in enforceLimits: a value that cannot fit under any circumstances must not
// spin, and must not be retried into an infinite eviction.
func TestAValueLargerThanTheBudgetDoesNotEmptyTheKeyspaceForever(t *testing.T) {
	withEviction(t, config.LRU, 5, 100000000)
	config.MaxMemory = 1024
	t.Cleanup(func() { config.MaxMemory = 0 })

	d := CreateDict()
	done := make(chan struct{})
	go func() {
		d.Put("huge", d.NewObj(strings.Repeat("v", 100000), 0, 0, 0))
		d.Put("small", d.NewObj("v", 0, 0, 0))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("enforceLimits did not terminate on a value larger than the budget")
	}
	assert.Contains(t, d.dictStore, "small", "the dictionary must still work afterwards")
}
