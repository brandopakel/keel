package data_structure

import (
	"fmt"
	"runtime"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHashSetReportsNewFieldsNotChangedOnes(t *testing.T) {
	h := NewHash()
	assert.True(t, h.Set("a", "1"), "a field not there before is new")
	assert.False(t, h.Set("a", "2"), "overwriting an existing field is not new, even when the value changes")
	assert.False(t, h.Set("a", "2"), "and writing the same value is not new either")

	got, ok := h.Get("a")
	assert.True(t, ok)
	assert.Equal(t, "2", got, "not new does not mean not written")
	assert.Equal(t, 1, h.Len())
}

func TestHashDelCountsOnlyWhatWasThere(t *testing.T) {
	h := NewHash()
	h.Set("a", "1")
	h.Set("b", "2")

	assert.Equal(t, 2, h.Del("a", "b", "never-existed"))
	assert.Equal(t, 0, h.Len())
	assert.Equal(t, 0, h.Del("a"), "deleting twice removes nothing the second time")
}

func TestHashEntriesPairFieldsWithValues(t *testing.T) {
	h := NewHash()
	want := map[string]string{"a": "1", "b": "2", "c": "3"}
	for f, v := range want {
		h.Set(f, v)
	}

	fields, values := h.Entries()
	assert.Len(t, fields, 3)
	assert.Len(t, values, 3)
	got := make(map[string]string, len(fields))
	for i, f := range fields {
		got[f] = values[i]
	}
	assert.Equal(t, want, got, "Entries walks once, so the two slices line up")
}

// TestHashMemUsageIsExactAboutWhatItTracks: the estimate has an unmeasurable
// part (the allocator's size classes) and a measurable one (the bytes of the
// fields and values). The measurable part must be exact, because it is the part
// that moves with the data.
func TestHashMemUsageIsExactAboutWhatItTracks(t *testing.T) {
	h := NewHash()
	empty := h.MemUsage()
	assert.Equal(t, uint64(hashBaseBytes), empty)

	h.Set("field", "value") // 5 + 5
	assert.Equal(t, empty+hashFieldOverhead+10, h.MemUsage())

	// An overwrite replaces the value's bytes rather than adding them, and
	// leaves the field's alone. Getting this wrong is the bug that made
	// used_memory wrap to 18 exabytes elsewhere in this server.
	h.Set("field", "a-much-longer-value")
	assert.Equal(t, empty+hashFieldOverhead+5+uint64(len("a-much-longer-value")), h.MemUsage())

	h.Del("field")
	assert.Equal(t, empty, h.MemUsage(),
		"removing everything must return exactly what was charged, or the counter drifts")
}

// TestHashMemUsageTracksRealHeap keeps hashFieldOverhead honest.
//
// It is a measured constant, not a derived one, so it can drift out of date -
// a change in how values are stored, or a new Go map implementation. Comparing
// the estimate against HeapAlloc catches that; reading the struct would not.
func TestHashMemUsageTracksRealHeap(t *testing.T) {
	for _, size := range []struct{ field, value int }{{10, 10}, {20, 20}, {20, 40}} {
		runtime.GC()
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)

		const n = 200000
		h := NewHash()
		for i := 0; i < n; i++ {
			h.Set(fmt.Sprintf("%0*d", size.field, i), fmt.Sprintf("%0*d", size.value, i))
		}

		runtime.GC()
		runtime.GC()
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		runtime.KeepAlive(h)

		actual := after.HeapAlloc - before.HeapAlloc
		ratio := float64(h.MemUsage()) / float64(actual)
		t.Logf("field=%-3d value=%-3d estimated=%-11d actual=%-11d ratio=%.3f",
			size.field, size.value, h.MemUsage(), actual, ratio)

		// The band is tolerance, not the measurement. Observed here is 0.992 to
		// 1.035; the bound is 0.90 to 1.10 because the allocator rounds every
		// field and value up to a size class, which no O(1) estimate can
		// follow, and because the ratio moves with the toolchain - the string
		// accounting shifted by nearly 40% between Go 1.21 and 1.22, which is
		// what forced the floor in go.mod. A band tight enough to match today's
		// reading would fail on a Go release rather than on a real drift.
		// Wide enough to survive that, narrow enough to catch a wrong constant:
		// 64 off by 20 reads 0.81 at the smallest sizes.
		assert.Greater(t, ratio, 0.90,
			"field=%d value=%d: estimate is %.0f%% of real heap, too far below to bound anything",
			size.field, size.value, ratio*100)
		assert.Less(t, ratio, 1.10,
			"field=%d value=%d: estimate is %.0f%% of real heap, so it over-counts",
			size.field, size.value, ratio*100)
	}
}

func TestHashMemUsageSurvivesChurn(t *testing.T) {
	h := NewHash()
	base := h.MemUsage()
	for round := 0; round < 50; round++ {
		for i := 0; i < 100; i++ {
			h.Set("f"+strconv.Itoa(i), "value-"+strconv.Itoa(round))
		}
		for i := 0; i < 100; i++ {
			h.Del("f" + strconv.Itoa(i))
		}
	}
	assert.Equal(t, base, h.MemUsage(),
		"5000 writes and 5000 deletes must leave the counter exactly where it started")
}
