package data_structure

import (
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

func TestCMSCountsExactlyWhenThereAreNoCollisions(t *testing.T) {
	c := CreateCMS(1000, 5)
	assert.EqualValues(t, 10, c.IncrBy("a", 10))
	assert.EqualValues(t, 20, c.IncrBy("a", 10))
	assert.EqualValues(t, 30, c.IncrBy("b", 30))
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
		c.IncrBy(item, n)
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

func TestCMSSaturatesInsteadOfWrapping(t *testing.T) {
	c := CreateCMS(16, 2)
	c.IncrBy("x", math.MaxUint32-5)
	assert.EqualValues(t, math.MaxUint32, c.IncrBy("x", 10), "an overflowing counter pins to the maximum")
	assert.EqualValues(t, math.MaxUint32, c.Count("x"))
	assert.EqualValues(t, uint64(math.MaxUint32)+5, c.TotalCount(), "the total keeps counting in 64 bits")
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
