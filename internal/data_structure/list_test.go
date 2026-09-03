package data_structure

import (
	"fmt"
	"runtime"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestListPushesAndPopsAtBothEnds(t *testing.T) {
	l := NewList()
	l.PushBack("b", "c")
	l.PushFront("a")
	assert.Equal(t, []string{"a", "b", "c"}, l.All())

	v, ok := l.PopFront()
	assert.True(t, ok)
	assert.Equal(t, "a", v)

	v, ok = l.PopBack()
	assert.True(t, ok)
	assert.Equal(t, "c", v)
	assert.Equal(t, []string{"b"}, l.All())

	_, _ = l.PopFront()
	_, ok = l.PopFront()
	assert.False(t, ok, "an empty list pops nothing rather than panicking")
	assert.Equal(t, 0, l.Len())
}

// TestListPushFrontOrdersLikeRedis: LPUSH a b c leaves c at the head, because
// each element is pushed in turn.
func TestListPushFrontOrdersLikeRedis(t *testing.T) {
	l := NewList()
	l.PushFront("a", "b", "c")
	assert.Equal(t, []string{"c", "b", "a"}, l.All())
}

func TestListIndexCountsFromEitherEnd(t *testing.T) {
	l := NewList()
	l.PushBack("a", "b", "c")

	for i, want := range map[int]string{0: "a", 1: "b", 2: "c", -1: "c", -2: "b", -3: "a"} {
		got, ok := l.Index(i)
		assert.True(t, ok, "index %d", i)
		assert.Equal(t, want, got, "index %d", i)
	}
	_, ok := l.Index(3)
	assert.False(t, ok, "past the end is absent, not an error")
	_, ok = l.Index(-4)
	assert.False(t, ok)
}

func TestListRangeClampsRatherThanFailing(t *testing.T) {
	l := NewList()
	l.PushBack("a", "b", "c", "d", "e")

	assert.Equal(t, []string{"a", "b", "c", "d", "e"}, l.Range(0, -1))
	assert.Equal(t, []string{"b", "c"}, l.Range(1, 2))
	assert.Equal(t, []string{"d", "e"}, l.Range(-2, -1))
	assert.Equal(t, []string{"a", "b", "c", "d", "e"}, l.Range(-100, 100),
		"a range wider than the list is the whole list")
	assert.Empty(t, l.Range(3, 1), "a backwards range is empty")
	assert.Empty(t, l.Range(10, 20), "and so is one past the end")
}

// TestListRingSurvivesWrapping drives the buffer around its own end many times,
// which is where an off-by-one in the modular arithmetic shows up.
func TestListRingSurvivesWrapping(t *testing.T) {
	l := NewList()
	for i := 0; i < 1000; i++ {
		l.PushBack(strconv.Itoa(i))
	}
	for round := 0; round < 5000; round++ {
		v, ok := l.PopFront()
		assert.True(t, ok)
		l.PushBack(v)
	}
	assert.Equal(t, 1000, l.Len())

	want := make([]string, 0, 1000)
	for i := 5000 % 1000; len(want) < 1000; i = (i + 1) % 1000 {
		want = append(want, strconv.Itoa(i))
	}
	assert.Equal(t, want, l.All(), "rotating a list 5000 times returns it to a known order")
}

func TestListSetReplacesInPlace(t *testing.T) {
	l := NewList()
	l.PushBack("a", "b", "c")
	assert.True(t, l.Set(1, "replaced"))
	assert.True(t, l.Set(-1, "last"))
	assert.False(t, l.Set(9, "nope"))
	assert.Equal(t, []string{"a", "replaced", "last"}, l.All())
}

// TestPoppingReleasesTheElement: the buffer outlives the element, so a slot
// left pointing at a popped value keeps it alive - a leak the estimate cannot
// see, because the estimate stopped counting it.
func TestPoppingReleasesTheElement(t *testing.T) {
	l := NewList()
	l.PushBack("front", "back")
	l.PopFront()
	l.PopBack()

	for i, slot := range l.buf {
		assert.Equal(t, "", slot, "slot %d still holds a popped element", i)
	}
}

func TestListShrinksItsBufferAfterDraining(t *testing.T) {
	l := NewList()
	for i := 0; i < 10000; i++ {
		l.PushBack(strconv.Itoa(i))
	}
	grown := len(l.buf)
	assert.Greater(t, grown, 8000)

	for i := 0; i < 9990; i++ {
		l.PopFront()
	}
	assert.Equal(t, 10, l.Len())
	assert.Less(t, len(l.buf), grown/10,
		"a list pushed to ten thousand and drained to ten must give the slots back")
	assert.Len(t, l.All(), 10, "and still hold what is left, in order")
}

func TestListMemUsageIsExactAboutWhatItTracks(t *testing.T) {
	l := NewList()
	base := l.MemUsage()

	l.PushBack("hello")
	assert.Equal(t,
		listBaseBytes+uint64(len(l.buf))*listSlotOverhead+listElemOverhead+5,
		l.MemUsage())

	l.PopBack()
	assert.Equal(t, listBaseBytes+uint64(len(l.buf))*listSlotOverhead, l.MemUsage(),
		"the elements are given back exactly; the buffer is charged until trim releases it")

	for i := 0; i < 500; i++ {
		l.PushBack("x")
	}
	for i := 0; i < 500; i++ {
		l.PopFront()
	}
	// Back to base plus the buffer trim refuses to shrink below. The floor is
	// deliberate - a list churning around empty would otherwise reallocate on
	// every other operation - and 8 slots is 128 bytes, which is why the
	// element accounting rather than the total is what has to return to zero.
	assert.Equal(t, 0, l.Len())
	assert.Equal(t, uint64(0), l.elemBytes,
		"500 pushes and 500 pops give back every element byte")
	assert.Equal(t, base+8*listSlotOverhead, l.MemUsage(),
		"and leave only the buffer floor behind")
	assert.Len(t, l.buf, 8)
}

func TestListMemUsageTracksRealHeap(t *testing.T) {
	for _, elemLen := range []int{10, 20, 40} {
		runtime.GC()
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)

		const n = 200000
		l := NewList()
		for i := 0; i < n; i++ {
			l.PushBack(fmt.Sprintf("%0*d", elemLen, i))
		}

		runtime.GC()
		runtime.GC()
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		runtime.KeepAlive(l)

		actual := after.HeapAlloc - before.HeapAlloc
		ratio := float64(l.MemUsage()) / float64(actual)
		t.Logf("elem=%-3d cap=%-7d estimated=%-10d actual=%-10d ratio=%.3f",
			elemLen, len(l.buf), l.MemUsage(), actual, ratio)

		assert.Greater(t, ratio, 0.90,
			"elem=%d: estimate is %.0f%% of real heap, too far below to bound anything", elemLen, ratio*100)
		assert.Less(t, ratio, 1.10,
			"elem=%d: estimate is %.0f%% of real heap, so it over-counts", elemLen, ratio*100)
	}
}

// TestListMemUsageHoldsAfterShrinking is why the buffer and the elements are
// charged separately. A single per-slot constant covering both fits a full list
// and over-counts one that has shrunk to the point where trim leaves it.
func TestListMemUsageHoldsAfterShrinking(t *testing.T) {
	runtime.GC()
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	l := NewList()
	for i := 0; i < 200000; i++ {
		l.PushBack(fmt.Sprintf("%010d", i))
	}
	for i := 0; i < 150000; i++ {
		l.PopFront()
	}

	runtime.GC()
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(l)

	actual := after.HeapAlloc - before.HeapAlloc
	ratio := float64(l.MemUsage()) / float64(actual)
	t.Logf("after draining to %d of cap %d: estimated=%d actual=%d ratio=%.3f",
		l.Len(), len(l.buf), l.MemUsage(), actual, ratio)
	assert.Greater(t, ratio, 0.90)
	assert.Less(t, ratio, 1.10)
}
