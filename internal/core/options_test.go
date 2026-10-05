package core

import (
	"testing"

	"github.com/brandopakel/keel/internal/data_structure"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withOptions changes the default engine's options for the rest of tb, and
// puts back the ones it found when tb ends. A test that changes them more than
// once calls it each time; the cleanups run last first, so the options the
// test began with are the ones left.
func withOptions(tb testing.TB, change func(*Options)) {
	tb.Helper()
	found := Configuration()
	o := found
	change(&o)
	require.NoError(tb, Configure(o))
	tb.Cleanup(func() { require.NoError(tb, Configure(found)) })
}

// TestOptionsDefaults: an engine given no options has no bound on its keys or
// its memory and evicts by LRU, as the owner decided for embedded use, with
// Redis's figures for the rest - the space's own defaults.
func TestOptionsDefaults(t *testing.T) {
	t.Parallel()
	require.Equal(t, data_structure.DefaultLimits(), Options{}.limits())
	limits := Options{}.limits()
	assert.Zero(t, limits.MaxKeys, "no key-count bound")
	assert.Zero(t, limits.MaxMemory, "no memory bound")
	assert.Equal(t, EvictLRU, limits.Eviction)

	e := newEngine(Options{})
	assert.Equal(t, data_structure.DefaultLimits(), e.space.Limits())
	assert.Contains(t, on(t, e, "INFO", "memory"), "maxmemory:0\r\nmaxmemory_human:0B\r\nmaxmemory_policy:allkeys-lru\r\n")
}

// TestOptionsTurnSettingsOff: zero is each setting's default, and a negative
// value turns off the settings that can be turned off, as the space spells it.
func TestOptionsTurnSettingsOff(t *testing.T) {
	t.Parallel()
	off := Options{LFULogFactor: Off, LFUDecayPeriod: Off, LCSMaxCells: Off}.limits()
	assert.Zero(t, off.LFULogFactor, "a factor of zero counts every access")
	assert.Zero(t, off.LFUDecayPeriod, "never decays")
	assert.Zero(t, off.LCSMaxCells, "no LCS bound")

	set := Options{MaxMemory: 1 << 20, MaxKeys: 3, Eviction: EvictLFU, EvictionSamples: 7,
		LFULogFactor: 4, LFUDecayPeriod: 50, LCSMaxCells: 99}.limits()
	assert.Equal(t, data_structure.Limits{Eviction: EvictLFU, MaxKeys: 3, MaxMemory: 1 << 20,
		EvictionSamples: 7, LFULogFactor: 4, LFUDecayPeriod: 50, LCSMaxCells: 99}, set)
}

// TestOptionsRefused: options no engine can be held to are refused, and leave
// the engine as it was.
func TestOptionsRefused(t *testing.T) {
	t.Parallel()
	e := newEngine(Options{MaxKeys: 10})
	for _, o := range []Options{{MaxKeys: -1}, {EvictionSamples: -1}, {Eviction: EvictionPolicy(3)}} {
		require.Error(t, e.configure(o), "%+v", o)
	}
	assert.Equal(t, Options{MaxKeys: 10}, e.options)
	assert.Equal(t, 10, e.space.MaxKeys())
	assert.Panics(t, func() { newEngine(Options{MaxKeys: -1}) })
}

// TestConfigureHoldsTheDefaultEngine: Configure is how the server's flags reach
// the default engine, and it reaches no other.
func TestConfigureHoldsTheDefaultEngine(t *testing.T) {
	own := newEngine(Options{})
	withOptions(t, func(o *Options) { o.MaxMemory, o.MaxKeys, o.Eviction = 4096, 5, EvictLFU })
	assert.Equal(t, Options{MaxMemory: 4096, MaxKeys: 5, Eviction: EvictLFU}, Configuration())
	assert.Equal(t, 5, data_structure.DefaultSpace.MaxKeys())
	assert.Equal(t, uint64(4096), data_structure.DefaultSpace.MaxMemory())
	assert.Equal(t, data_structure.DefaultLimits(), own.space.Limits(), "another engine keeps its own")
	assert.Contains(t, run(t, "INFO", "memory"), "maxmemory:4096\r\nmaxmemory_human:4.00K\r\nmaxmemory_policy:allkeys-lfu\r\n")
}
