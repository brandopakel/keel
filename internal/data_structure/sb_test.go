package data_structure

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

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
}

func TestSBChainGrowsWhenAFilterFills(t *testing.T) {
	sb := CreateSBChain(10, 0.01, 2)
	for i := 0; i < 50; i++ {
		assert.True(t, sb.Add(fmt.Sprintf("%d", i)), "item %d is new", i)
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
	assert.True(t, sb.Add("x"))
	assert.False(t, sb.Add("x"), "a second add is not an addition")
	assert.EqualValues(t, 1, sb.Count(), "and is not counted")
	assert.Equal(t, 1, sb.Filters())
}

func TestSBChainDuplicatesAreFoundAcrossFilters(t *testing.T) {
	sb := CreateSBChain(5, 0.01, 2)
	for i := 0; i < 5; i++ {
		sb.Add(fmt.Sprintf("%d", i))
	}
	sb.Add("spill") // opens a second filter
	assert.Equal(t, 2, sb.Filters())
	assert.False(t, sb.Add("0"), "an item in an older filter is still known")
	assert.EqualValues(t, 6, sb.Count())
}

func TestSBChainMemUsageCountsEveryFilter(t *testing.T) {
	sb := CreateSBChain(10, 0.01, 2)
	one := sb.MemUsage()
	assert.Equal(t, uint64(sbChainBaseBytes)+bloomBaseBytes+sb.filters[0].bloom.bytes, one)
	for i := 0; i < 11; i++ {
		sb.Add(fmt.Sprintf("%d", i))
	}
	assert.Equal(t, 2, sb.Filters())
	assert.Greater(t, sb.MemUsage(), one, "a second filter costs more")
}

func TestSBChainFalsePositivesStayBounded(t *testing.T) {
	// With tightening ratio r and initial rate p, the chain's rate converges
	// to p/(1-r), which at the defaults is 2%.
	sb := CreateSBChain(100, 0.01, 2)
	for i := 0; i < 20000; i++ {
		sb.Add(fmt.Sprintf("in-%d", i))
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
