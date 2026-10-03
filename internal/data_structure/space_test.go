package data_structure

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/brandopakel/keel/internal/config"
)

// TestSpacesShareNothing is the property a Space exists for: two of them in one
// process evict, clock and report removals independently.
func TestSpacesShareNothing(t *testing.T) {
	t.Parallel()
	full, idle := newTestDict(evictionLimits(config.LRU, 5, 10)), newTestDict(evictionLimits(config.LRU, 5, 10))
	var fullRemoved, idleRemoved []string
	full.space.OnRemove = func(_, key string) { fullRemoved = append(fullRemoved, key) }
	idle.space.OnRemove = func(_, key string) { idleRemoved = append(idleRemoved, key) }

	idle.Put("kept", idle.NewObj("v"))
	idleClock := idle.space.clock
	for i := 0; i < 100; i++ {
		full.Put("k"+strconv.Itoa(i), full.NewObj("v"))
	}

	require.Equal(t, 10, full.Len(), "the full space is held to its own limit")
	require.EqualValues(t, 90, full.space.Evicted())
	require.Len(t, fullRemoved, 90, "its hook hears every eviction")
	require.Equal(t, 1, idle.Len())
	require.Zero(t, idle.space.Evicted(), "eviction in one space must not touch another")
	require.Empty(t, idleRemoved)
	require.Equal(t, idleClock, idle.space.clock, "accesses in one space must not age keys in another")
}

// TestSpaceLimitsAreItsOwn checks a limit set on one space binds that space
// alone.
func TestSpaceLimitsAreItsOwn(t *testing.T) {
	t.Parallel()
	tight := configuredLimits()
	tight.LCSMaxCells = 10
	require.True(t, NewSpace(tight).LCSTooLarge("abcd", "abcd"))
	require.False(t, NewSpace(configuredLimits()).LCSTooLarge("abcd", "abcd"))
}

// TestDefaultSpaceReadsConfigLive pins how the server's space keeps working
// with the flags and the tests that assign config: it holds no copy, and reads
// each setting when it is used. A space of its own ignores config entirely.
//
// Serial on purpose, because it assigns config: Go finishes every serial test
// before it starts a parallel one, so no test reading config can see this.
func TestDefaultSpaceReadsConfigLive(t *testing.T) {
	strategy, keys, memory, samples := config.EvictStrategy, config.KeyNumberLimit, config.MaxMemory, config.LRUSamples
	logFactor, decay, cells := config.LFULogFactor, config.LFUDecayPeriod, config.LCSMaxCells
	t.Cleanup(func() {
		config.EvictStrategy, config.KeyNumberLimit, config.MaxMemory, config.LRUSamples = strategy, keys, memory, samples
		config.LFULogFactor, config.LFUDecayPeriod, config.LCSMaxCells = logFactor, decay, cells
	})
	own := NewSpace(Limits{EvictStrategy: config.LRU, KeyNumberLimit: 1, MaxMemory: 2, LRUSamples: 3,
		LFULogFactor: 4, LFUDecayPeriod: 5, LCSMaxCells: 6})

	config.EvictStrategy, config.KeyNumberLimit, config.MaxMemory = config.LFU, 11, 12
	config.LRUSamples, config.LFULogFactor, config.LFUDecayPeriod, config.LCSMaxCells = 13, 14, 15, 16
	read := func(s *Space) []any {
		l := s.limits
		return []any{*l.evictStrategy, *l.keyNumberLimit, *l.maxMemory, *l.lruSamples,
			*l.lfuLogFactor, *l.lfuDecayPeriod, *l.lcsMaxCells}
	}
	require.Equal(t, []any{config.LFU, 11, uint64(12), 13, 14, 15, uint64(16)}, read(DefaultSpace))
	require.Equal(t, []any{config.LRU, 1, uint64(2), 3, 4, 5, uint64(6)}, read(own))
	require.EqualValues(t, evictionSeed, own.rng, "every space starts the generator at the same seed")
}
