package data_structure

import (
	"math"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
)

// relErr is the error of an estimate against the truth, as a fraction.
func relErr(estimate uint64, actual int) float64 {
	if actual == 0 {
		return float64(estimate)
	}
	return math.Abs(float64(estimate)-float64(actual)) / float64(actual)
}

// tolerance is well above the 0.81% standard error this configuration gives.
// The inputs below are fixed and the hash is seeded, so every estimate here is
// deterministic - this bound is headroom for future changes, not for flakiness.
const tolerance = 0.03

// TestRegisterPackingRoundTrips exhaustively checks the 6-bit packing.
//
// Registers do not align with bytes: three in four straddle a boundary, so a
// read or write has to work through a 16-bit window. Getting the shift or the
// mask wrong corrupts a neighbouring register rather than the one addressed,
// which shows up later as a wrong estimate and not as an obvious failure. Every
// register at every value is only 16384*64 checks, so there is no reason to
// sample.
func TestRegisterPackingRoundTrips(t *testing.T) {
	h := CreateHLL()
	for val := uint8(0); val <= hllRegisterMax; val++ {
		for i := 0; i < hllRegisters; i++ {
			h.setRegister(i, val)
		}
		for i := 0; i < hllRegisters; i++ {
			if got := h.getRegister(i); got != val {
				t.Fatalf("register %d: wrote %d, read %d", i, val, got)
			}
		}
	}
}

// TestRegisterWriteDoesNotDisturbNeighbours is the failure the packing invites:
// a mask that is too wide silently rewrites the register next door.
func TestRegisterWriteDoesNotDisturbNeighbours(t *testing.T) {
	h := CreateHLL()
	for i := 0; i < hllRegisters; i++ {
		h.setRegister(i, uint8(i%(hllRegisterMax+1)))
	}
	for _, target := range []int{0, 1, 2, 3, 4, 1000, hllRegisters - 2, hllRegisters - 1} {
		h.setRegister(target, 63)
		for i := 0; i < hllRegisters; i++ {
			want := uint8(i % (hllRegisterMax + 1))
			if i == target {
				want = 63
			}
			if got := h.getRegister(i); got != want {
				t.Fatalf("writing register %d disturbed register %d: want %d, got %d",
					target, i, want, got)
			}
		}
		h.setRegister(target, uint8(target%(hllRegisterMax+1)))
	}
}

func TestCountOfEmptySketchIsZero(t *testing.T) {
	assert.Equal(t, uint64(0), CreateHLL().Count())
}

// TestSmallCardinalitiesAreExact matters because the estimator's whole
// difficulty is at the ends of the range. The original algorithm needed linear
// counting bolted on below ~2.5m to avoid being badly wrong here.
func TestSmallCardinalitiesAreExact(t *testing.T) {
	for _, n := range []int{1, 2, 3, 5, 10, 50} {
		h := CreateHLL()
		for i := 0; i < n; i++ {
			h.Add("item:" + strconv.Itoa(i))
		}
		assert.Equal(t, uint64(n), h.Count(), "small cardinalities should be exact")
	}
}

func TestAccuracyAcrossMagnitudes(t *testing.T) {
	for _, n := range []int{100, 1000, 10000, 100000, 1000000} {
		h := CreateHLL()
		for i := 0; i < n; i++ {
			h.Add("item:" + strconv.Itoa(i))
		}
		e := relErr(h.Count(), n)
		assert.LessOrEqual(t, e, tolerance,
			"n=%d estimated %d, relative error %.3f%%", n, h.Count(), e*100)
	}
}

// TestAddIsIdempotent pins the property the whole structure rests on: a register
// only moves up, so re-adding an item is a no-op. Without it the estimate would
// depend on how many times each item appeared, which is a frequency count, not
// a cardinality.
func TestAddIsIdempotent(t *testing.T) {
	h := CreateHLL()
	for i := 0; i < 1000; i++ {
		h.Add("item:" + strconv.Itoa(i))
	}
	before := h.Count()

	changed := false
	for round := 0; round < 3; round++ {
		for i := 0; i < 1000; i++ {
			if h.Add("item:" + strconv.Itoa(i)) {
				changed = true
			}
		}
	}
	assert.False(t, changed, "re-adding known items must not move any register")
	assert.Equal(t, before, h.Count(), "re-adding known items must not change the estimate")
}

func TestAddReportsWhetherSketchChanged(t *testing.T) {
	h := CreateHLL()
	assert.True(t, h.Add("first"), "a new item should change the sketch")
	assert.False(t, h.Add("first"), "the same item again should not")
}

// TestMergeEqualsAddingEverything is the strong statement of correctness for
// union: merging two sketches must produce exactly the sketch that adding all
// the items to one would have produced. Not approximately - the same bytes.
func TestMergeEqualsAddingEverything(t *testing.T) {
	a, b, both := CreateHLL(), CreateHLL(), CreateHLL()
	for i := 0; i < 20000; i++ {
		item := "left:" + strconv.Itoa(i)
		a.Add(item)
		both.Add(item)
	}
	for i := 0; i < 30000; i++ {
		item := "right:" + strconv.Itoa(i)
		b.Add(item)
		both.Add(item)
	}
	a.Merge(b)
	assert.Equal(t, both.regs, a.regs, "merged registers must match a sketch built from all items")
	assert.Equal(t, both.Count(), a.Count())
}

func TestMergeOfDisjointSetsApproximatesTheSum(t *testing.T) {
	a, b := CreateHLL(), CreateHLL()
	for i := 0; i < 50000; i++ {
		a.Add("left:" + strconv.Itoa(i))
	}
	for i := 0; i < 50000; i++ {
		b.Add("right:" + strconv.Itoa(i))
	}
	a.Merge(b)
	assert.LessOrEqual(t, relErr(a.Count(), 100000), tolerance)
}

// TestMergeOfOverlappingSetsCountsTheUnion checks that a union is a union and
// not a sum: two sets sharing every element must not estimate to twice the size.
func TestMergeOfOverlappingSetsCountsTheUnion(t *testing.T) {
	a, b := CreateHLL(), CreateHLL()
	for i := 0; i < 40000; i++ {
		a.Add("item:" + strconv.Itoa(i))
		b.Add("item:" + strconv.Itoa(i))
	}
	a.Merge(b)
	assert.LessOrEqual(t, relErr(a.Count(), 40000), tolerance,
		"merging identical sets must not double the estimate")
}

// TestCountCacheIsInvalidatedByWrites guards the optimisation: Count is O(m) so
// the result is cached, and a stale cache would silently freeze the estimate.
func TestCountCacheIsInvalidatedByWrites(t *testing.T) {
	h := CreateHLL()
	for i := 0; i < 100; i++ {
		h.Add("item:" + strconv.Itoa(i))
	}
	first := h.Count()
	assert.Equal(t, first, h.Count(), "a cached count must be stable when nothing changes")

	for i := 100; i < 10000; i++ {
		h.Add("item:" + strconv.Itoa(i))
	}
	assert.Greater(t, h.Count(), first, "the cache must be dropped when a register moves")

	other := CreateHLL()
	for i := 100000; i < 150000; i++ {
		other.Add("item:" + strconv.Itoa(i))
	}
	afterAdds := h.Count()
	h.Merge(other)
	assert.Greater(t, h.Count(), afterAdds, "Merge must drop the cache too")
}

// TestPatLenStaysInRange pins the sentinel bit in patLen. Without it a hash
// whose upper bits are all zero would scan off the end and produce a register
// value that does not fit in six bits, corrupting its neighbour on write.
func TestPatLenStaysInRange(t *testing.T) {
	for _, hash := range []uint64{0, 1, math.MaxUint64, 1 << 63, (1 << hllP) - 1, 1 << hllP} {
		index, count := patLen(hash)
		assert.GreaterOrEqual(t, index, 0)
		assert.Less(t, index, hllRegisters)
		assert.GreaterOrEqual(t, count, uint8(1))
		assert.LessOrEqual(t, count, uint8(hllQ+1), "a register value must fit in six bits")
		assert.LessOrEqual(t, count, uint8(hllRegisterMax))
	}
}

func TestMemoryFootprintIsTwelveKB(t *testing.T) {
	h := CreateHLL()
	assert.Equal(t, 12*1024+1, len(h.regs), "16384 six-bit registers, plus the spare window byte")
}
