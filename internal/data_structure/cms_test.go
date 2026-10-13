package data_structure

import (
	"encoding/hex"
	"fmt"
	"math"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCMSDimensions(t *testing.T) {
	c := CreateCMS(10, 20)
	assert.EqualValues(t, 10, c.Width())
	assert.EqualValues(t, 20, c.Depth())
	assert.Len(t, c.counter, 200)
	assert.Zero(t, c.TotalCount())
	assert.Equal(t, cmsBaseBytes+uint64(200*4), c.MemUsage())
	assert.Equal(t, c.MemUsage(), CMSMemUsageFor(10, 20))
}

func TestCalcCMSDim(t *testing.T) {
	w, d := CalcCMSDim(0.001, 0.001)
	assert.EqualValues(t, 2000, w, "width is 2/epsilon")
	assert.EqualValues(t, 10, d, "depth is log2(1/delta)")
	w, d = CalcCMSDim(0.01, 0.01)
	assert.EqualValues(t, 200, w)
	assert.EqualValues(t, 7, d)
	w, d = CalcCMSDim(0.5, 0.5)
	assert.EqualValues(t, 4, w)
	assert.EqualValues(t, 1, d)
}

func incr(c *CMS, item string, n uint64) uint32 { return c.IncrBy(item, n) }

func TestCMSCountsExactlyWhenThereAreNoCollisions(t *testing.T) {
	c := CreateCMS(1000, 5)
	assert.EqualValues(t, 10, incr(c, "a", 10))
	assert.EqualValues(t, 20, incr(c, "a", 10))
	assert.EqualValues(t, 30, incr(c, "b", 30))
	assert.EqualValues(t, 20, c.Count("a"))
	assert.EqualValues(t, 30, c.Count("b"))
	assert.EqualValues(t, 0, c.Count("never"))
	assert.EqualValues(t, 50, c.TotalCount())
}

// TestCMSNeverUnderestimates is the property the structure promises: a
// collision can push a counter up but never down, so an estimate is at least
// the truth, and with the sketch sized for it, not far above.
func TestCMSNeverUnderestimates(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	w, d := CalcCMSDim(0.01, 0.001)
	c := CreateCMS(w, d)

	truth := map[string]uint32{}
	var total uint64
	for i := 0; i < 50000; i++ {
		// A skewed stream, so a few items carry most of the count.
		item := fmt.Sprintf("k%d", int(math.Abs(rng.NormFloat64())*300))
		n := uint32(1 + rng.Intn(5))
		truth[item] += n
		total += uint64(n)
		c.IncrBy(item, uint64(n))
	}
	assert.Equal(t, total, c.TotalCount())

	over := 0
	for item, want := range truth {
		got := c.Count(item)
		assert.GreaterOrEqual(t, got, want, "%s", item)
		if uint64(got-want) > uint64(0.01*float64(total)) {
			over++
		}
	}
	assert.Less(t, float64(over)/float64(len(truth)), 0.001*10,
		"estimates past the error bound should be rarer than delta allows, with room")
}

// TestCMSSaturatesAsRedisBloomDoes: a counter that cannot take an increment
// stops at the maximum rather than wrapping, and an estimate of the maximum
// is the overflow RedisBloom reports, reached exactly or not, an increment
// of 2^32 or more included.
func TestCMSSaturatesAsRedisBloomDoes(t *testing.T) {
	c := CreateCMS(16, 2)
	assert.EqualValues(t, math.MaxUint32-5, c.IncrBy("x", math.MaxUint32-5))
	assert.EqualValues(t, math.MaxUint32, c.IncrBy("x", 5), "reaching the maximum exactly")
	assert.EqualValues(t, math.MaxUint32, c.IncrBy("x", 0))
	assert.EqualValues(t, math.MaxUint32, c.IncrBy("x", 10), "an overflowing counter pins to the maximum")
	assert.EqualValues(t, math.MaxUint32, c.Count("x"))
	assert.EqualValues(t, math.MaxUint32, c.IncrBy("y", 1<<32), "an increment past 32 bits")
	assert.EqualValues(t, math.MaxUint32, c.IncrBy("z", math.MaxInt64))
	assert.EqualValues(t, uint64(math.MaxUint32)+10+(1<<32)+math.MaxInt64, c.TotalCount())
}

// TestCMSMergeIsRedisBloomsWeightedSum: MergeFrom sets each counter to the
// weighted sum of the sources' and the total to theirs, the destination
// among the sources included, and refuses a sum that leaves a counter's 32
// bits or goes negative, leaving the destination as it was.
func TestCMSMergeIsRedisBloomsWeightedSum(t *testing.T) {
	a, b, dest := CreateCMS(100, 3), CreateCMS(100, 3), CreateCMS(100, 3)
	a.IncrBy("x", 5)
	b.IncrBy("x", 7)
	b.IncrBy("y", 1)
	assert.NoError(t, dest.MergeFrom([]*CMS{a, b}, []int64{1, 1}))
	assert.EqualValues(t, 12, dest.Count("x"))
	assert.EqualValues(t, 1, dest.Count("y"))
	assert.EqualValues(t, 13, dest.TotalCount())
	assert.NoError(t, dest.MergeFrom([]*CMS{dest, a}, []int64{2, -1}))
	assert.EqualValues(t, 19, dest.Count("x"), "the destination as a source, its weight applied before it is overwritten")
	assert.EqualValues(t, 21, dest.TotalCount())
	assert.ErrorIs(t, dest.MergeFrom([]*CMS{a}, []int64{-1}), ErrCMSMergeOverflow, "a negative counter")
	assert.ErrorIs(t, dest.MergeFrom([]*CMS{a}, []int64{1 << 32}), ErrCMSMergeOverflow, "a counter past 32 bits")
	assert.EqualValues(t, 19, dest.Count("x"), "a refused merge leaves the destination")
}

// TestCMSReadsAPriorBuildsState decodes a sketch marshalled by the build before
// this one: the counters have to land in the same cells under the current
// hashing, or a restored sketch would answer about the wrong items.
func TestCMSReadsAPriorBuildsState(t *testing.T) {
	const fixture = "10000000030000000800000000000000000000000000000000000000000000000000000000000000000000000000000000000000050000000000000000000000000000000300000000000000000000000000000000000000000000000300000000000000000000000000000000000000050000000000000000000000000000000000000000000000000000000000000000000000030000000500000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000"
	raw, err := hex.DecodeString(fixture)
	assert.NoError(t, err)
	c, err := UnmarshalCMS(raw)
	assert.NoError(t, err)
	assert.EqualValues(t, 16, c.Width())
	assert.EqualValues(t, 3, c.Depth())
	assert.EqualValues(t, 8, c.TotalCount())
	assert.EqualValues(t, 3, c.Count("alpha"))
	assert.EqualValues(t, 5, c.Count("beta"))
	assert.EqualValues(t, 0, c.Count("gamma"))

	// The cells themselves, pinned: these are where the fixture's counters sit.
	assert.Equal(t, []uint64{13, 19, 33}, []uint64{c.cell("alpha", 0), c.cell("alpha", 1), c.cell("alpha", 2)})
	assert.Equal(t, []uint64{9, 24, 34}, []uint64{c.cell("beta", 0), c.cell("beta", 1), c.cell("beta", 2)})
}

func TestCMSCellsStayInsideTheirRow(t *testing.T) {
	c := CreateCMS(7, 3)
	for i := 0; i < 1000; i++ {
		item := fmt.Sprintf("%d", i)
		for row := uint32(0); row < 3; row++ {
			at := c.cell(item, row)
			assert.GreaterOrEqual(t, at, uint64(row*7))
			assert.Less(t, at, uint64((row+1)*7))
		}
	}
}
