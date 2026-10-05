package core

import (
	"fmt"

	"github.com/brandopakel/keel/internal/data_structure"
)

// Options are an engine's settings.
//
// They replace the package variables of internal/config, which every engine in
// a process shared (plan step 2.5). The server's flags are mapped onto them in
// cmd/keel, which passes every value it has explicitly; the defaults below are
// for a caller that leaves a field out, such as a library embedding Keel.
//
// The zero value of every field is its default, so Options{} is an engine
// with no bound on its keys or its memory and LRU eviction, as the owner
// decided for embedded use (docs/embedding-plan.md, "Decisions"), and Redis's
// figures for the rest. A setting that can be turned off, where off is not its
// default, is turned off with a negative value.
type Options struct {
	// MaxMemory bounds the keyspace in estimated bytes, as Redis's maxmemory
	// does; see data_structure.Limits. Zero is no bound.
	MaxMemory uint64

	// MaxKeys bounds the keyspace by count: once it is reached, a write
	// evicts before it lands. Zero is no bound, as in Redis, where only
	// maxmemory bounds the keyspace. The server's -maxkeys flag sets it, and
	// its default is the server's cap of 5,000,000 keys. A log rewrite refuses
	// a keyspace over 4,000,000 keys whatever this is (see RewriteKeyCeiling).
	MaxKeys int

	// Eviction is the policy a bound evicts by. The zero value is EvictLRU.
	Eviction EvictionPolicy

	// EvictionSamples is how many keys an LRU or LFU eviction samples. Zero
	// is 5, as Redis's maxmemory-samples defaults to.
	EvictionSamples int

	// LFULogFactor is how slowly LFU's logarithmic access counter rises. Zero
	// is 10, as Redis's lfu-log-factor defaults to; a negative factor is a
	// factor of zero, which counts every access.
	LFULogFactor int

	// LFUDecayPeriod is how many accesses pass before an idle key's LFU
	// counter drops by one. Zero is 10,000; a negative period never decays.
	LFUDecayPeriod int

	// LCSMaxCells bounds len(key1)*len(key2) for LCS. Zero is 134,217,728,
	// where Redis stops; a negative bound is no bound.
	LCSMaxCells int64
}

// EvictionPolicy is how an engine over one of its bounds chooses the keys to
// evict.
type EvictionPolicy = data_structure.EvictionPolicy

// The eviction policies, as INFO names them: allkeys-lru, allkeys-lfu and
// allkeys-random.
const (
	EvictLRU    = data_structure.EvictLRU
	EvictLFU    = data_structure.EvictLFU
	EvictRandom = data_structure.EvictRandom
)

// validate refuses options no engine can be held to. Each refusal names the
// field, for a caller that builds Options directly; cmd/keel checks its flags
// before it maps them, with the messages the server has always given.
func (o Options) validate() error {
	switch {
	case o.MaxKeys < 0:
		return fmt.Errorf("MaxKeys must not be negative, got %d", o.MaxKeys)
	case o.Eviction != EvictLRU && o.Eviction != EvictLFU && o.Eviction != EvictRandom:
		return fmt.Errorf("unknown Eviction policy %d", o.Eviction)
	case o.EvictionSamples < 0:
		return fmt.Errorf("EvictionSamples must not be negative, got %d", o.EvictionSamples)
	}
	return nil
}

// limits are the space's limits these options describe: every default filled
// in, and every setting turned off spelled as the space spells it, zero.
func (o Options) limits() data_structure.Limits {
	limits := data_structure.DefaultLimits()
	limits.Eviction = o.Eviction
	limits.MaxKeys = o.MaxKeys
	limits.MaxMemory = o.MaxMemory
	if o.EvictionSamples != 0 {
		limits.EvictionSamples = o.EvictionSamples
	}
	switch {
	case o.LFULogFactor < 0:
		limits.LFULogFactor = 0
	case o.LFULogFactor > 0:
		limits.LFULogFactor = o.LFULogFactor
	}
	switch {
	case o.LFUDecayPeriod < 0:
		limits.LFUDecayPeriod = 0
	case o.LFUDecayPeriod > 0:
		limits.LFUDecayPeriod = o.LFUDecayPeriod
	}
	switch {
	case o.LCSMaxCells < 0:
		limits.LCSMaxCells = 0
	case o.LCSMaxCells > 0:
		limits.LCSMaxCells = uint64(o.LCSMaxCells)
	}
	return limits
}

// Off is the value that turns off a setting whose zero is its default:
// LFUDecayPeriod never decays, LCSMaxCells has no bound, and LFULogFactor
// counts every access. Any negative value does the same.
const Off = -1

// configure holds e to o from now on: its space's limits are o's, and what
// e reports as its options is o, as given. Nothing is changed when o is
// refused.
func (e *Engine) configure(o Options) error {
	if err := o.validate(); err != nil {
		return err
	}
	e.options = o
	e.space.SetLimits(o.limits())
	return nil
}

// Configure holds the default engine, the server's, to o. cmd/keel calls it
// once with the options its flags describe, before the log is replayed or
// anything is served, as it once assigned config before anything read it.
func Configure(o Options) error { return defaultEngine.configure(o) }

// Configuration is the default engine's options, as last given to Configure:
// Options{} until then.
func Configuration() Options { return defaultEngine.options }
