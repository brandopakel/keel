package data_structure

import (
	"math"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
)

// exponentsOf reads the raw cell values behind an item, so a test can combine
// the rows differently from the way Count does.
func (m *Morris) exponentsOf(item string) []uint8 {
	exps := make([]uint8, m.depth)
	for row := uint32(0); row < m.depth; row++ {
		exps[row] = m.counters[m.cell(item, row)]
	}
	return exps
}

func (m *Morris) countByMin(item string) uint64 {
	lowest := uint8(morrisMaxExponent)
	for _, c := range m.exponentsOf(item) {
		if c < lowest {
			lowest = c
		}
	}
	return morrisValue[lowest]
}

// TestEstimateTableIsTheStatedFormula pins the two derived tables against the
// definition, so a change to morrisA cannot quietly leave them inconsistent.
func TestEstimateTableIsTheStatedFormula(t *testing.T) {
	assert.Equal(t, uint64(0), morrisValue[0], "an untouched counter must read zero")
	assert.Equal(t, uint64(1), morrisValue[1], "the first increment must read one")
	assert.Equal(t, 1.0, morrisProb[0], "the first increment must always take")

	for _, c := range []int{2, 7, 40, 128, 255} {
		want := (math.Pow(1+morrisA, float64(c)) - 1) / morrisA
		assert.InDelta(t, want, float64(morrisValue[c]), 1, "value table at c=%d", c)
		assert.InDelta(t, 1/(1+want*morrisA), morrisProb[c], 1e-12, "probability table at c=%d", c)
	}
}

// TestEightBitsHoldTheCountMinRange is the reason morrisA is 0.08: the point of
// the structure is that one byte holds what a Count-Min sketch spends four on.
//
// It also guards the initialisation order. MorrisMaxCount reads a table that
// init fills, and a package-level initialiser would have run first and captured
// a zero - which is exactly what it did before this test existed.
func TestEightBitsHoldTheCountMinRange(t *testing.T) {
	assert.Equal(t, uint64(4168383430), MorrisMaxCount,
		"eight bits reach four billion, as the package comment claims")
	assert.InEpsilon(t, float64(math.MaxUint32), float64(MorrisMaxCount), 0.03,
		"which is within 3% of what a uint32 Count-Min cell holds")
	assert.InDelta(t, 0.20, MorrisRelativeError, 0.005,
		"the accuracy paid for that range")
}

// TestSingleCounterIsUnbiased is the property the whole construction rests on.
//
// Any individual counter is wrong - that is the trade - but the falling
// increment probability is chosen so the expected estimate is the true count.
// Averaged over enough independent counters the estimate must converge on the
// truth, and the spread must be the promised sqrt(a/2).
func TestSingleCounterIsUnbiased(t *testing.T) {
	const trials = 4000
	for _, truth := range []uint64{10, 1000, 1000000} {
		var sum, sumSq float64
		for i := 0; i < trials; i++ {
			m := CreateMorris(1, 1)
			m.rngState = uint64(i)*0x9E3779B97F4A7C15 + 12345
			m.bump(0, truth)
			est := float64(morrisValue[m.counters[0]])
			sum += est
			sumSq += est * est
		}
		mean := sum / trials
		sd := math.Sqrt(sumSq/trials - mean*mean)

		// The mean of `trials` samples has standard error sd/sqrt(trials), so
		// four of those is a bound the truth clears with room to spare.
		tolerance := 4 * sd / math.Sqrt(trials)
		assert.InDelta(t, float64(truth), mean, tolerance,
			"estimate must be unbiased at a true count of %d", truth)

		assert.InDelta(t, MorrisRelativeError, sd/mean, 0.05,
			"relative spread at a true count of %d must be about sqrt(a/2)", truth)
	}
}

// TestBulkIncrementMatchesRepeatedIncrement checks the geometric shortcut.
//
// bump draws the wait to the next success rather than flipping a coin per
// increment. That is only legitimate if it produces the same distribution, so
// one call of n must be indistinguishable from n calls of one.
func TestBulkIncrementMatchesRepeatedIncrement(t *testing.T) {
	const trials = 3000
	const truth = 50000

	mean := func(bulk bool) float64 {
		var sum float64
		for i := 0; i < trials; i++ {
			m := CreateMorris(1, 1)
			m.rngState = uint64(i)*0xBF58476D1CE4E5B9 + 999
			if bulk {
				m.bump(0, truth)
			} else {
				for j := 0; j < truth; j++ {
					m.bump(0, 1)
				}
			}
			sum += float64(morrisValue[m.counters[0]])
		}
		return sum / trials
	}

	bulk, oneAtATime := mean(true), mean(false)
	assert.InEpsilon(t, oneAtATime, bulk, 0.06,
		"drawing the geometric wait must match flipping every coin")
}

// TestCounterSaturatesRatherThanWrapping. A wrap would turn the most-counted
// item into the least-counted one, which is the worst possible failure for
// something whose only job is to rank.
func TestMorrisCounterSaturatesRatherThanWrapping(t *testing.T) {
	m := CreateMorris(1, 1)
	for i := 0; i < 64; i++ {
		m.bump(0, math.MaxUint64/64)
	}
	assert.Equal(t, uint8(morrisMaxExponent), m.counters[0])
	assert.Equal(t, MorrisMaxCount, morrisValue[m.counters[0]])
}

func TestEmptyTableCountsNothing(t *testing.T) {
	m := CreateMorris(100, 5)
	assert.Equal(t, uint64(0), m.Count("anything"))
	assert.Equal(t, uint64(0), m.TotalCount())
}

// TestMedianBeatsMinimumAcrossRows is the measurement behind the choice of
// combiner, run on a table wide enough that collisions are negligible so that
// what is left is the noise of the counters themselves.
//
// The minimum is what a Count-Min sketch would take. It must come out
// systematically low, and must get worse as rows are added: every extra row is
// another chance for one of them to read low, so depth - which is supposed to
// buy accuracy - spends it instead.
func TestMedianBeatsMinimumAcrossRows(t *testing.T) {
	const width = 20000
	const items = 500
	const each = 5000

	bias := func(depth uint32) (median, minimum float64) {
		m := CreateMorris(width, depth)
		for i := 0; i < items; i++ {
			m.IncrBy("item:"+strconv.Itoa(i), each)
		}
		for i := 0; i < items; i++ {
			item := "item:" + strconv.Itoa(i)
			median += (float64(m.Count(item)) - each) / each
			minimum += (float64(m.countByMin(item)) - each) / each
		}
		return median / items, minimum / items
	}

	var lastMin float64
	for _, depth := range []uint32{5, 7, 9} {
		median, minimum := bias(depth)
		t.Logf("depth=%d mean relative error: median %+.1f%%, minimum %+.1f%%",
			depth, 100*median, 100*minimum)

		assert.Less(t, math.Abs(median), 0.05,
			"the median must stay near unbiased at depth %d", depth)
		assert.Less(t, minimum, -0.15,
			"the minimum must be systematically low at depth %d", depth)
		if lastMin != 0 {
			assert.Less(t, minimum, lastMin,
				"the minimum must get worse as rows are added, not better")
		}
		lastMin = minimum
	}
}

// TestCollidedRowsDoNotSinkTheEstimate is the other half of the combiner
// argument: the median has to ignore a minority of contaminated rows, or it
// would be no better than a mean.
func TestCollidedRowsDoNotSinkTheEstimate(t *testing.T) {
	m := CreateMorris(64, 9) // deliberately narrow, so collisions are certain
	for i := 0; i < 2000; i++ {
		m.IncrBy("noise:"+strconv.Itoa(i), 100)
	}
	m.IncrBy("tracked", 100000)

	est := float64(m.Count("tracked"))
	assert.Greater(t, est, 50000.0, "a heavily collided table must still rank the heavy hitter above the noise")
	for i := 0; i < 50; i++ {
		assert.Less(t, float64(m.Count("noise:"+strconv.Itoa(i))), est,
			"every light item must estimate below the heavy one")
	}
}

func TestMemUsageIsOneBytePerCell(t *testing.T) {
	m := CreateMorris(2000, 7)
	assert.Equal(t, uint64(morrisBaseBytes)+2000*7, m.MemUsage())

	// The comparison that justifies the structure: same shape, quarter the cost.
	cms := CreateCMS(2000, 7)
	assert.Equal(t, uint64(cmsBaseBytes)+2000*7*4, cms.MemUsage())
	assert.Less(t, m.MemUsage()*3, cms.MemUsage())
}

func TestDimensionsMatchTheCountMinSketch(t *testing.T) {
	for _, errRate := range []float64{0.01, 0.001} {
		for _, errProb := range []float64{0.1, 0.01} {
			mw, md := CalcMorrisDim(errRate, errProb)
			cw, cd := CalcCMSDim(errRate, errProb)
			assert.Equal(t, cw, mw)
			assert.Equal(t, cd, md)
		}
	}
}
