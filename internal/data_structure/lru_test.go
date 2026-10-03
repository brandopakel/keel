package data_structure

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/brandopakel/keel/internal/config"
)

// newTestDict builds a dictionary registered as the only keyspace of a space
// of its own.
//
// Eviction spans every keyspace registered in a space, so a dictionary that is
// not registered is invisible to it. A space per test is what lets these tests
// run in parallel: none of them shares a clock, a pool or a limit with another.
func newTestDict(limits Limits) *Dict {
	space := NewSpace(limits)
	d := CreateDict(space)
	space.RegisterKeyspace(d)
	return d
}

// evictionLimits are the configured defaults with the eviction knobs a test
// sets. Reading config is safe from a parallel test: the one test here that
// assigns it is serial, and Go finishes serial tests before parallel ones.
func evictionLimits(strategy, samples, limit int) Limits {
	return Limits{
		EvictStrategy:  strategy,
		KeyNumberLimit: limit,
		MaxMemory:      config.MaxMemory,
		LRUSamples:     samples,
		LFULogFactor:   config.LFULogFactor,
		LFUDecayPeriod: config.LFUDecayPeriod,
		LCSMaxCells:    config.LCSMaxCells,
	}
}

// configuredLimits are the defaults, for a test that sets nothing.
func configuredLimits() Limits {
	return evictionLimits(config.EvictStrategy, config.LRUSamples, config.KeyNumberLimit)
}

func TestAccessUpdatesRecency(t *testing.T) {
	t.Parallel()
	d := newTestDict(configuredLimits())
	d.Put("a", d.NewObj("v"))
	d.Put("b", d.NewObj("v"))

	first := d.Peek("a").Access
	assert.Greater(t, d.Peek("b").Access, first, "a later write is more recent")

	d.Get("a")
	assert.Greater(t, d.Peek("a").Access, d.Peek("b").Access,
		"reading a key must make it the most recently used")
}

func TestNoEvictionBelowTheLimit(t *testing.T) {
	t.Parallel()
	d := newTestDict(evictionLimits(config.LRU, 5, 100))
	for i := 0; i < 100; i++ {
		d.Put("k"+strconv.Itoa(i), d.NewObj("v"))
	}
	assert.Equal(t, 100, d.Len(), "nothing should be evicted until the limit is reached")
}

// TestOverwritingAnExistingKeyDoesNotEvict guards a subtle waste: a Put that
// replaces a key does not grow the dictionary, so evicting to make room for it
// throws a key away for nothing.
func TestOverwritingAnExistingKeyDoesNotEvict(t *testing.T) {
	t.Parallel()
	d := newTestDict(evictionLimits(config.LRU, 5, 10))
	for i := 0; i < 10; i++ {
		d.Put("k"+strconv.Itoa(i), d.NewObj("v"))
	}
	assert.Equal(t, 10, d.Len())

	for i := 0; i < 50; i++ {
		d.Put("k3", d.NewObj("updated"))
	}
	assert.Equal(t, 10, d.Len(), "overwriting must not evict")
	for i := 0; i < 10; i++ {
		assert.NotNil(t, d.Peek("k"+strconv.Itoa(i)), "k%d should still be present", i)
	}
}

func TestEvictionHoldsTheDictAtTheLimit(t *testing.T) {
	t.Parallel()
	d := newTestDict(evictionLimits(config.LRU, 5, 100))
	for i := 0; i < 1000; i++ {
		d.Put("k"+strconv.Itoa(i), d.NewObj("v"))
		assert.LessOrEqual(t, d.Len(), 100, "the dict must never exceed its limit")
	}
	assert.Equal(t, 100, d.Len())
}

// hotRetention fills a dict to the limit, marks the first half recently used,
// then forces exactly that many evictions and reports what fraction of the hot
// keys survived. True LRU would score 100%: it would evict only cold keys.
func hotRetention(t *testing.T, strategy, samples, limit int) float64 {
	t.Helper()
	d := newTestDict(evictionLimits(strategy, samples, limit))
	for i := 0; i < limit; i++ {
		d.Put("k"+strconv.Itoa(i), d.NewObj("v"))
	}
	hot := limit / 2
	for i := 0; i < hot; i++ {
		d.Get("k" + strconv.Itoa(i))
	}
	for i := 0; i < hot; i++ {
		d.Put("new"+strconv.Itoa(i), d.NewObj("v"))
	}

	survived := 0
	for i := 0; i < hot; i++ {
		if d.Peek("k"+strconv.Itoa(i)) != nil {
			survived++
		}
	}
	return float64(survived) / float64(hot) * 100
}

// TestApproxLRUKeepsHotKeys is the point of the policy. Measured retention with
// five samples sits at 82-84%; the bound below is loose enough that map
// iteration order cannot make it flake, and tight enough to fail if the pool or
// the recency tracking stops working.
func TestApproxLRUKeepsHotKeys(t *testing.T) {
	t.Parallel()
	assert.Greater(t, hotRetention(t, config.LRU, 5, 10000), 75.0)
}

// TestApproxLRUBeatsRandomEviction is the comparison that justifies the policy
// existing at all: with no recency information, eviction keeps hot keys only in
// proportion to how many there are.
func TestApproxLRUBeatsRandomEviction(t *testing.T) {
	t.Parallel()
	random := hotRetention(t, config.EvictFirst, 5, 10000)
	lru := hotRetention(t, config.LRU, 5, 10000)

	assert.Less(t, random, 65.0, "random eviction should be near chance, got %.1f%%", random)
	assert.Greater(t, lru-random, 20.0,
		"approximate LRU should beat random by a wide margin: %.1f%% vs %.1f%%", lru, random)
}

// TestMoreSamplesImproveRetention pins the trade-off the policy is named for:
// accuracy is bought with sampling work, and the knob is real.
func TestMoreSamplesImproveRetention(t *testing.T) {
	t.Parallel()
	few := hotRetention(t, config.LRU, 2, 10000)
	many := hotRetention(t, config.LRU, 20, 10000)
	assert.Greater(t, many, few,
		"20 samples should retain more than 2: %.1f%% vs %.1f%%", many, few)
	assert.Greater(t, many, 90.0)
}

// TestEvictionSkipsCandidatesReadSinceSampling covers why pool entries record
// the access time they were sampled at. A key can be read between being pooled
// and being evicted, and evicting it then would throw away the most recently
// used key in the dictionary.
func TestEvictionSkipsCandidatesReadSinceSampling(t *testing.T) {
	t.Parallel()
	d := newTestDict(evictionLimits(config.LRU, 5, 100))
	for i := 0; i < 20; i++ {
		d.Put("k"+strconv.Itoa(i), d.NewObj("v"))
	}

	// k0 is read last, so it is the newest key in the dict...
	d.Get("k0")
	// ...but the pool still holds the stale reading from when it was cold.
	d.space.pool = []Candidate{{Keyspace: d, Key: "k0", Score: 1}}

	before := d.Len()
	d.space.evictOne()

	assert.Equal(t, before-1, d.Len(), "an eviction must still remove exactly one key")
	assert.NotNil(t, d.Peek("k0"),
		"the most recently used key must not be evicted on a stale pool entry")
}

func TestEvictionSkipsCandidatesAlreadyDeleted(t *testing.T) {
	t.Parallel()
	d := newTestDict(evictionLimits(config.LRU, 5, 100))
	for i := 0; i < 20; i++ {
		d.Put("k"+strconv.Itoa(i), d.NewObj("v"))
	}
	d.space.pool = []Candidate{{Keyspace: d, Key: "gone", Score: 1}}

	before := d.Len()
	d.space.evictOne()
	assert.Equal(t, before-1, d.Len(), "a stale candidate must not stop eviction making room")
}

func TestPoolStaysSortedAndBounded(t *testing.T) {
	t.Parallel()
	d := newTestDict(configuredLimits())
	s := d.space
	for i := 0; i < 100; i++ {
		// insert in an order that is neither ascending nor descending
		s.poolInsert(Candidate{Keyspace: d, Key: "k" + strconv.Itoa(i), Score: uint64((i * 37) % 100)})
	}
	assert.LessOrEqual(t, len(s.pool), evictionPoolSize, "the pool must stay bounded")
	for i := 1; i < len(s.pool); i++ {
		assert.LessOrEqual(t, s.pool[i-1].Score, s.pool[i].Score,
			"the pool must stay ordered oldest first")
	}
	// It should be holding the oldest candidates it saw, not just any of them.
	assert.Less(t, s.pool[len(s.pool)-1].Score, uint64(evictionPoolSize+1))
}

// TestPoolDoesNotHoldOneKeyTwice matters because a duplicated candidate makes
// the second attempt to evict it a guaranteed miss, wasting a pool slot.
func TestPoolDoesNotHoldOneKeyTwice(t *testing.T) {
	t.Parallel()
	d := newTestDict(configuredLimits())
	s := d.space
	s.poolInsert(Candidate{Keyspace: d, Key: "dup", Score: 5})
	s.poolInsert(Candidate{Keyspace: d, Key: "dup", Score: 3})
	s.poolInsert(Candidate{Keyspace: d, Key: "dup", Score: 9})

	count := 0
	for _, e := range s.pool {
		if e.Key == "dup" {
			count++
		}
	}
	assert.Equal(t, 1, count, "a key must appear in the pool at most once")
	assert.Equal(t, uint64(9), s.pool[0].Score, "the entry must carry the latest reading")
}
