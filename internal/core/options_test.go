package core

import (
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/brandopakel/keel/internal/data_structure"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withOptionsOn changes e's options for the rest of tb, and puts back the
// ones it found when tb ends. A test that changes them more than once calls it
// each time; the cleanups run last first, so the options the test began with
// are the ones left.
func withOptionsOn(tb testing.TB, e *Engine, change func(*Options)) {
	tb.Helper()
	found := e.options
	reconfigure(tb, e, change)
	tb.Cleanup(func() { require.NoError(tb, e.configure(found)) })
}

// reconfigure changes the options of e, an engine the test made, from now on.
func reconfigure(tb testing.TB, e *Engine, change func(*Options)) {
	tb.Helper()
	o := e.options
	change(&o)
	require.NoError(tb, e.configure(o))
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
	for _, o := range []Options{{MaxKeys: -1}, {EvictionSamples: -1}, {Eviction: EvictionPolicy(3)},
		{ActiveExpirePercent: -1}, {ActiveExpirePercent: 101}, {ActiveExpireRounds: -1},
		{Fsync: "sometimes"}, {AppendOnly: true}, {ReplicationProtocol: 3}, {ReplicationProtocol: -1}} {
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
	found := Configuration()
	t.Cleanup(func() { require.NoError(t, Configure(found)) })
	require.NoError(t, Configure(Options{MaxMemory: 4096, MaxKeys: 5, Eviction: EvictLFU}))
	assert.Equal(t, Options{MaxMemory: 4096, MaxKeys: 5, Eviction: EvictLFU}, Configuration())
	assert.Equal(t, 5, data_structure.DefaultSpace.MaxKeys())
	assert.Equal(t, uint64(4096), data_structure.DefaultSpace.MaxMemory())
	assert.Equal(t, data_structure.DefaultLimits(), own.space.Limits(), "another engine keeps its own")
	assert.Contains(t, runOn(t, defaultEngine, "INFO", "memory"), "maxmemory:4096\r\nmaxmemory_human:4.00K\r\nmaxmemory_policy:allkeys-lfu\r\n")
}

// TestOptionsResolveSettings: what the engine reads is its options with every
// default filled in and every setting that is off spelled as zero, and an
// engine held to WithDefaults() is held to exactly what it was.
func TestOptionsResolveSettings(t *testing.T) {
	t.Parallel()
	assert.Equal(t, settings{expireSamples: 20, expirePercent: 25, expireRounds: 16, fsync: FsyncEverySec,
		rewritePercentage: 100, rewriteMinSize: 64 << 20}, Options{}.settings())
	assert.Equal(t, replicationRole{Protocol: 1}, Options{}.role())

	off := Options{ActiveExpireSamples: Off, AutoRewritePercentage: Off, AutoRewriteMinSize: Off}.settings()
	assert.Zero(t, off.expireSamples, "no active expiry")
	assert.Zero(t, off.rewritePercentage, "no automatic rewrite")
	assert.Zero(t, off.rewriteMinSize, "no minimum size")

	e := newEngine(Options{ActiveExpireSamples: 3, ActiveExpirePercent: 50, ActiveExpireRounds: 2,
		Fsync: FsyncAlways, AsyncAppend: true, AutoRewritePercentage: 10, AutoRewriteMinSize: 99,
		ReplicaOf: "primary.test:6379", ReplicationFeed: true, ReplicationProtocol: 2})
	assert.Equal(t, settings{expireSamples: 3, expirePercent: 50, expireRounds: 2, fsync: FsyncAlways,
		asyncAppend: true, rewritePercentage: 10, rewriteMinSize: 99}, e.settings)
	assert.Equal(t, replicationRole{ReplicaOf: "primary.test:6379", Feed: true, Protocol: 2}, e.role)

	for _, o := range []Options{{}, e.options,
		{LFULogFactor: Off, LFUDecayPeriod: Off, LCSMaxCells: Off, ActiveExpireSamples: Off,
			AutoRewritePercentage: Off, AutoRewriteMinSize: Off},
		{MaxKeys: 9, MaxMemory: 9, AppendOnly: true, AppendFilename: "x.aof"}} {
		held := o.WithDefaults()
		assert.Equal(t, held, held.WithDefaults(), "%+v", o)
		assert.Equal(t, o.limits(), held.limits(), "%+v", o)
		assert.Equal(t, o.settings(), held.settings(), "%+v", o)
		assert.Equal(t, o.role(), held.role(), "%+v", o)
	}
}

// TestEnginesShareNoOptions: an engine's settings are its own. Two engines
// held to different options, and a third held to neither's, the defaults,
// each expire, sync and report only as their own options say.
func TestEnginesShareNoOptions(t *testing.T) {
	t.Parallel()
	third := newTestEngine(t, Options{})
	lazy := newTestEngine(t, Options{ActiveExpireSamples: Off, Fsync: FsyncAlways, MaxMemory: 1 << 20, Eviction: EvictLFU})
	eager := newTestEngine(t, Options{Fsync: FsyncNever, ReplicationFeed: true, ReplicationProtocol: 2})
	dir := t.TempDir()
	var syncs [2]atomic.Int64
	for i, e := range []*Engine{lazy, eager} {
		e.aofSync = func(f *os.File) error { syncs[i].Add(1); return f.Sync() }
		require.NoError(t, e.OpenAOF(filepath.Join(dir, strconv.Itoa(i)+".aof")))
	}
	for _, e := range []*Engine{lazy, eager, third} {
		for i := 0; i < 50; i++ {
			on(t, e, "SET", "k"+strconv.Itoa(i), "v", "PX", "1")
		}
	}
	waitPast(10)
	assert.Zero(t, lazy.ExpireCycle(), "active expiry is off on this engine alone")
	assert.Positive(t, eager.ExpireCycle())
	assert.Positive(t, third.ExpireCycle(), "the third engine expires as its own options say")

	require.Equal(t, "OK", on(t, lazy, "SET", "durable", "v"))
	require.Equal(t, "OK", on(t, eager, "SET", "durable", "v"))
	for _, e := range []*Engine{lazy, eager} {
		require.NoError(t, e.FlushAOF())
	}
	assert.Positive(t, syncs[0].Load(), "always syncs this engine's log")
	assert.Zero(t, syncs[1].Load(), "no leaves this one's to the system")

	assert.Contains(t, on(t, lazy, "INFO", "memory"), "maxmemory:1048576\r\nmaxmemory_human:1.00M\r\nmaxmemory_policy:allkeys-lfu\r\n")
	assert.Contains(t, on(t, eager, "INFO", "memory"), "maxmemory:0\r\nmaxmemory_human:0B\r\nmaxmemory_policy:allkeys-lru\r\n")
	assert.Contains(t, on(t, eager, "INFO", "replication"), "replication_protocol:2\r\n")
	assert.Contains(t, on(t, lazy, "INFO", "replication"), "replication_protocol:1\r\n")
	assert.Equal(t, Options{}, third.options, "neither reaches the third engine")
	assert.Equal(t, data_structure.DefaultLimits(), third.space.Limits())
}
