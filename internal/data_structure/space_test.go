package data_structure

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSpacesShareNothing is the property a Space exists for: two of them in one
// process evict, clock and report removals independently.
func TestSpacesShareNothing(t *testing.T) {
	t.Parallel()
	full, idle := newTestDict(evictionLimits(EvictLRU, 5, 10)), newTestDict(evictionLimits(EvictLRU, 5, 10))
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
	tight := DefaultLimits()
	tight.LCSMaxCells = 10
	require.True(t, NewSpace(tight).LCSTooLarge("abcd", "abcd"))
	require.False(t, NewSpace(DefaultLimits()).LCSTooLarge("abcd", "abcd"))
}

// TestSpaceLimitsAreHeldByValue pins where a space reads its limits: from
// itself, as set, so that setting one space's limits reaches no other space,
// the default space's included, and a copy of the limits a caller keeps is not
// a way to change them.
func TestSpaceLimitsAreHeldByValue(t *testing.T) {
	t.Parallel()
	set := Limits{Eviction: EvictLFU, MaxKeys: 1, MaxMemory: 2, EvictionSamples: 3,
		LFULogFactor: 4, LFUDecayPeriod: 5, LCSMaxCells: 6}
	own, other := NewSpace(set), NewSpace(DefaultLimits())
	require.Equal(t, set, own.Limits())
	require.Equal(t, 1, own.MaxKeys())
	require.Equal(t, uint64(2), own.MaxMemory())

	copied := own.Limits()
	copied.MaxKeys = 100
	require.Equal(t, 1, own.MaxKeys(), "a copy of the limits does not change them")

	set.MaxKeys, set.Eviction = 7, EvictRandom
	own.SetLimits(set)
	require.Equal(t, set, own.Limits())
	require.Equal(t, DefaultLimits(), other.Limits(), "setting one space's limits reaches no other")
	require.EqualValues(t, evictionSeed, own.rng, "every space starts the generator at the same seed")
}

// TestDefaultLimits pins the limits of a space nobody has set any for: no
// bound on keys or memory, as in Redis, LRU, and Redis's sampling, LFU and
// LCS figures. An engine made with no options holds its space to them.
func TestDefaultLimits(t *testing.T) {
	t.Parallel()
	require.Equal(t, Limits{Eviction: EvictLRU, MaxKeys: 0, MaxMemory: 0, EvictionSamples: 5,
		LFULogFactor: 10, LFUDecayPeriod: 10000, LCSMaxCells: 134217728}, DefaultLimits())
	require.Zero(t, EvictLRU, "the zero policy is LRU")
}

// TestUnboundedSpaceNeverEvicts: with neither bound set, a space keeps every
// key it is given, and enforcing its limits evicts nothing.
func TestUnboundedSpaceNeverEvicts(t *testing.T) {
	t.Parallel()
	for _, policy := range []EvictionPolicy{EvictLRU, EvictLFU, EvictRandom} {
		d := newTestDict(evictionLimits(policy, 5, 0))
		for i := 0; i < 1000; i++ {
			d.Put("k"+strconv.Itoa(i), d.NewObj("v"))
		}
		d.space.EnforceLimits()
		require.Equal(t, 1000, d.Len(), "policy %d", policy)
		require.Zero(t, d.space.Evicted(), "policy %d", policy)
	}
}
