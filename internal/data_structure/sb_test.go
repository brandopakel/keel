package data_structure

import (
	"encoding/hex"
	"fmt"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
)

// add is Add without the error, for the many cases where growth cannot fail.
func add(sb *SBChain, item string) bool {
	added, err := sb.Add(item)
	if err != nil {
		panic(err)
	}
	return added
}

func TestSBChainStartsWithOneFilter(t *testing.T) {
	sb := CreateSBChain(10, 0.01, 2)
	assert.Equal(t, 1, sb.Filters())
	assert.EqualValues(t, 10, sb.Capacity())
	assert.EqualValues(t, 0, sb.Count())
	assert.EqualValues(t, 2, sb.Expansion())
	assert.EqualValues(t, 0.01, sb.filters[0].bloom.Error)

	assert.Nil(t, CreateSBChain(0, 0.01, 2), "no filter can be sized for nothing")
	assert.Nil(t, CreateSBChain(10, 0, 2))
	assert.Nil(t, CreateSBChain(10, 1, 2))
	assert.Nil(t, CreateSBChain(10, 0.01, 0), "a chain that grew by nothing would grow an empty filter")
	assert.Nil(t, CreateSBChain(1<<40, 0.01, 2), "a first filter past the per-key cap is refused")
	assert.NotNil(t, CreateSBChain(100000000, 0.01, 2), "a hundred million items at one percent fits under it")
}

// TestSBChainRefusesToGrowPastTheCap: an expansion of a few billion turned the
// second distinct item into a multi-gigabyte allocation. Growth is sized first
// now, and a filter past the cap is refused with the chain left as it was.
func TestSBChainRefusesToGrowPastTheCap(t *testing.T) {
	sb := CreateSBChain(1, 0.01, 4294967295)
	assert.NotNil(t, sb)
	assert.True(t, add(sb, "first"))

	added, err := sb.Add("second")
	assert.False(t, added)
	assert.ErrorIs(t, err, ErrFilterTooLarge)
	assert.Equal(t, 1, sb.Filters(), "nothing was allocated")
	assert.EqualValues(t, 1, sb.Count())
	assert.True(t, sb.Exists("first"), "what was there is still there")
	assert.False(t, sb.Exists("second"))

	added, err = sb.Add("first")
	assert.False(t, added)
	assert.NoError(t, err, "an item already present needs no growth, so is not refused")

	// The overflow guard: a capacity that would wrap saturates and is refused
	// rather than wrapping to something small.
	huge := &SBChain{growthFactor: 1 << 40}
	huge.grow(1<<30, 0.5)
	huge.newest().size = 1 << 30
	capacity, _ := huge.nextFilter()
	assert.EqualValues(t, uint64(math.MaxUint64), capacity)
}

// TestSBChainReadsAPriorBuildsState decodes a chain marshalled by the build
// before this one and checks it still knows its items. The bytes are committed
// rather than produced by the code under test, so a change to the hash, the
// seed or the field layout fails here rather than after a restart.
func TestSBChainReadsAPriorBuildsState(t *testing.T) {
	const fixture = "06000000000000000200000000000000020000000000000004000000000000000700000004000000000000007b14ae47e17a843f85168ac58c2b23404000000000000000080000000000000086c40428041d0c2202000000000000000800000008000000000000007b14ae47e17a743fe3862fb2350e2640800000000000000010000000000000000c040000000404040800040404000000"
	raw, err := hex.DecodeString(fixture)
	assert.NoError(t, err)
	sb, err := UnmarshalSBChain(raw)
	assert.NoError(t, err)

	assert.Equal(t, 2, sb.Filters(), "four items filled the first filter of capacity 4, two went into the second")
	assert.EqualValues(t, 6, sb.Count())
	assert.EqualValues(t, 12, sb.Capacity())
	assert.EqualValues(t, 2, sb.Expansion())
	for _, item := range []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta"} {
		assert.True(t, sb.Exists(item), "%s was in the filter when it was written", item)
		assert.False(t, add(sb, item), "and is still known to be, so is not added again")
	}
	for _, probe := range []string{"eta", "theta", "iota", "kappa", "lambda", "mu", "nu", "xi"} {
		assert.False(t, sb.Exists(probe), "%s was not, and was checked not to collide when the fixture was made", probe)
	}
	// It goes on working after the restore.
	assert.True(t, add(sb, "omega"))
	assert.True(t, sb.Exists("omega"))
	assert.EqualValues(t, 7, sb.Count())
}

func TestSBChainGrowsWhenAFilterFills(t *testing.T) {
	sb := CreateSBChain(10, 0.01, 2)
	for i := 0; i < 50; i++ {
		assert.True(t, add(sb, fmt.Sprintf("%d", i)), "item %d is new", i)
	}
	// Ten in the first filter, twenty in the second, the last twenty in a
	// third sized for forty.
	assert.Equal(t, 3, sb.Filters())
	assert.EqualValues(t, 10+20+40, sb.Capacity())
	assert.EqualValues(t, 50, sb.Count())
	assert.EqualValues(t, 10, sb.filters[0].size)
	assert.EqualValues(t, 20, sb.filters[1].size)
	assert.EqualValues(t, 20, sb.filters[2].size)

	// Each new filter is larger by the expansion and stricter by the
	// tightening ratio, which is what keeps the chain's error bounded.
	assert.EqualValues(t, 10, sb.filters[0].bloom.Entries)
	assert.EqualValues(t, 20, sb.filters[1].bloom.Entries)
	assert.EqualValues(t, 40, sb.filters[2].bloom.Entries)
	assert.EqualValues(t, 0.01, sb.filters[0].bloom.Error)
	assert.EqualValues(t, 0.005, sb.filters[1].bloom.Error)
	assert.EqualValues(t, 0.0025, sb.filters[2].bloom.Error)

	for i := 0; i < 50; i++ {
		assert.True(t, sb.Exists(fmt.Sprintf("%d", i)))
	}
	assert.False(t, sb.Exists("not-added"))
}

func TestSBChainAddReportsDuplicates(t *testing.T) {
	sb := CreateSBChain(100, 0.01, 2)
	assert.True(t, add(sb, "x"))
	assert.False(t, add(sb, "x"), "a second add is not an addition")
	assert.EqualValues(t, 1, sb.Count(), "and is not counted")
	assert.Equal(t, 1, sb.Filters())
}

func TestSBChainDuplicatesAreFoundAcrossFilters(t *testing.T) {
	sb := CreateSBChain(5, 0.01, 2)
	for i := 0; i < 5; i++ {
		add(sb, fmt.Sprintf("%d", i))
	}
	add(sb, "spill") // opens a second filter
	assert.Equal(t, 2, sb.Filters())
	assert.False(t, add(sb, "0"), "an item in an older filter is still known")
	assert.EqualValues(t, 6, sb.Count())
}

func TestSBChainMemUsageCountsEveryFilter(t *testing.T) {
	sb := CreateSBChain(10, 0.01, 2)
	one := sb.MemUsage()
	assert.Equal(t, uint64(sbChainBaseBytes)+bloomBaseBytes+sb.filters[0].bloom.bytes, one)
	for i := 0; i < 11; i++ {
		add(sb, fmt.Sprintf("%d", i))
	}
	assert.Equal(t, 2, sb.Filters())
	assert.Greater(t, sb.MemUsage(), one, "a second filter costs more")
}

func TestSBChainFalsePositivesStayBounded(t *testing.T) {
	// With tightening ratio r and initial rate p, the chain's rate converges
	// to p/(1-r), which at the defaults is 2%.
	sb := CreateSBChain(100, 0.01, 2)
	for i := 0; i < 20000; i++ {
		add(sb, fmt.Sprintf("in-%d", i))
	}
	hits := 0
	const probes = 50000
	for i := 0; i < probes; i++ {
		if sb.Exists(fmt.Sprintf("out-%d", i)) {
			hits++
		}
	}
	assert.Less(t, float64(hits)/probes, 0.02*1.5, "%d filters", sb.Filters())
}
