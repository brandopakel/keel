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
// default, is turned off with a negative value: Off.
type Options struct {
	// The keyspace's bounds, and the policy that keeps it inside them.

	// MaxMemory bounds the keyspace in estimated bytes, as Redis's maxmemory
	// does; see data_structure.Limits. Zero is no bound.
	MaxMemory uint64

	// MaxKeys bounds the keyspace by count: once it is reached, a write
	// evicts before it lands. Zero is no bound, as in Redis, where only
	// maxmemory bounds the keyspace. The server's -maxkeys flag sets it, and
	// defaults to zero too. A log rewrite refuses a keyspace over 4,000,000
	// keys whatever this is (see RewriteKeyCeiling).
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

	// Active expiry: how hard the engine looks for keys whose TTL has passed
	// rather than waiting for something to read them.
	//
	// The sampling is Redis's. ActiveExpireSamples keys with a TTL are
	// examined; if more than ActiveExpirePercent of them had fallen due, the
	// keyspace probably holds many more, and another round is drawn. Rounds
	// are capped at ActiveExpireRounds, so one cycle cannot become a long
	// stall on a keyspace that is mostly expired: what is left over is found
	// on the next cycle.

	// ActiveExpireSamples is how many keys with a TTL one round examines.
	// Zero is 20; a negative count turns active expiry off, leaving expiry
	// lazy.
	ActiveExpireSamples int

	// ActiveExpirePercent is the share of a round's sample, in percent, that
	// has to have fallen due for another round to be drawn. Zero is 25.
	ActiveExpirePercent int

	// ActiveExpireRounds is the most rounds one cycle draws. Zero is 16.
	ActiveExpireRounds int

	// The append-only log. Off by default, as it is in Redis: it costs a
	// write per event-loop cycle and, under FsyncAlways, a disk flush before
	// every reply.

	// AppendOnly says the engine keeps its log in AppendFilename, replayed
	// when it starts. The server's startup reads both (server.StartAOF), until
	// core.Open runs that sequence (plan phase 3); a caller that opens a log
	// itself, with OpenAOF, sets neither.
	AppendOnly bool

	// AppendFilename is where the log lives. It has no default: AppendOnly
	// needs one. The server's -appendfilename flag defaults to
	// ./keel-master.aof.
	AppendFilename string

	// Fsync is how often the log is flushed to disk. Zero is FsyncEverySec.
	Fsync FsyncPolicy

	// AsyncAppend appends the log on a worker, with one batch of commands of
	// back-pressure, rather than on the caller's thread.
	AsyncAppend bool

	// AutoRewritePercentage is how much the log grows past its size after the
	// last rewrite before it is rewritten automatically. The size after a
	// rewrite is roughly the size the data needs, and growth past it is
	// history: commands superseded by later ones, and keys since deleted. Zero
	// is 100, Redis's default, which rewrites once the log has doubled; a
	// negative percentage never rewrites automatically. BGREWRITEAOF still
	// works.
	AutoRewritePercentage int

	// AutoRewriteMinSize is the least size, in bytes, the log is rewritten
	// automatically at, which stops a small log being rewritten constantly. A
	// 64 MiB log takes a moment to replay and costs nothing to keep, so
	// doubling from 1KB to 2KB is not worth a rewrite even though it is 100%
	// growth. Zero is 64 MiB, Redis's default; a negative size is no minimum.
	AutoRewriteMinSize int64

	// What the engine is in replication. Replication is experimental, and
	// the server requires an authenticated log for it.

	// ReplicaOf is the primary the engine follows as a read-only replica, as
	// host:port; empty, the engine is no replica.
	ReplicaOf string

	// ReplicationFeed says the engine feeds a stream of its writes to
	// replicas of its own.
	ReplicationFeed bool

	// ReplicationProtocol is the protocol the engine speaks to its primary or
	// its replicas: 1, images, or 2, streaming snapshots, operation deltas and
	// recovery checkpoints. Zero is 1.
	ReplicationProtocol int
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

// FsyncPolicy is how often the log is flushed to disk.
//
// FsyncEverySec is the default for the reason Redis chose it: FsyncAlways
// turns every event-loop cycle into a disk round trip, and on a server whose
// commands run one at a time that is a stall every other connection shares.
type FsyncPolicy string

// The fsync policies, by the names Redis's appendfsync takes.
const (
	// FsyncAlways syncs before replying, so an acknowledged write is a
	// durable one.
	FsyncAlways FsyncPolicy = "always"
	// FsyncEverySec syncs at most once a second; a crash loses up to a
	// second.
	FsyncEverySec FsyncPolicy = "everysec"
	// FsyncNever leaves it to the operating system.
	FsyncNever FsyncPolicy = "no"
)

// Off is the value that turns off a setting whose zero is its default:
// LFUDecayPeriod never decays, LCSMaxCells has no bound, LFULogFactor counts
// every access, ActiveExpireSamples leaves expiry lazy, AutoRewritePercentage
// never rewrites automatically, and AutoRewriteMinSize has no minimum. Any
// negative value does the same.
const Off = -1

// The defaults of the settings whose zero is not taken as written. The
// keyspace's are the space's own, data_structure.DefaultLimits.
const (
	defaultActiveExpireSamples   = 20
	defaultActiveExpirePercent   = 25
	defaultActiveExpireRounds    = 16
	defaultAutoRewritePercentage = 100
	defaultAutoRewriteMinSize    = int64(64 << 20)
	defaultReplicationProtocol   = 1
)

// WithDefaults returns o with every field that is zero, and has a default,
// set to that default. A setting that is off stays Off, and a setting with
// no default (MaxKeys, MaxMemory, AppendFilename, ReplicaOf and the flags)
// stays as it is, so an engine held to WithDefaults() is held to exactly
// what it was held to by o.
func (o Options) WithDefaults() Options {
	limits := data_structure.DefaultLimits()
	or := func(v, def int) int {
		if v == 0 {
			return def
		}
		return v
	}
	o.EvictionSamples = or(o.EvictionSamples, limits.EvictionSamples)
	o.LFULogFactor = or(o.LFULogFactor, limits.LFULogFactor)
	o.LFUDecayPeriod = or(o.LFUDecayPeriod, limits.LFUDecayPeriod)
	if o.LCSMaxCells == 0 {
		o.LCSMaxCells = int64(limits.LCSMaxCells)
	}
	o.ActiveExpireSamples = or(o.ActiveExpireSamples, defaultActiveExpireSamples)
	o.ActiveExpirePercent = or(o.ActiveExpirePercent, defaultActiveExpirePercent)
	o.ActiveExpireRounds = or(o.ActiveExpireRounds, defaultActiveExpireRounds)
	if o.Fsync == "" {
		o.Fsync = FsyncEverySec
	}
	o.AutoRewritePercentage = or(o.AutoRewritePercentage, defaultAutoRewritePercentage)
	if o.AutoRewriteMinSize == 0 {
		o.AutoRewriteMinSize = defaultAutoRewriteMinSize
	}
	o.ReplicationProtocol = or(o.ReplicationProtocol, defaultReplicationProtocol)
	return o
}

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
	case o.ActiveExpirePercent < 0 || o.ActiveExpirePercent > 100:
		return fmt.Errorf("ActiveExpirePercent must be from 0 to 100, got %d", o.ActiveExpirePercent)
	case o.ActiveExpireRounds < 0:
		return fmt.Errorf("ActiveExpireRounds must not be negative, got %d", o.ActiveExpireRounds)
	case o.Fsync != "" && o.Fsync != FsyncAlways && o.Fsync != FsyncEverySec && o.Fsync != FsyncNever:
		return fmt.Errorf("unknown Fsync policy %q (want always, everysec or no)", o.Fsync)
	case o.AppendOnly && o.AppendFilename == "":
		return fmt.Errorf("AppendOnly needs an AppendFilename")
	case o.ReplicationProtocol != 0 && o.ReplicationProtocol != 1 && o.ReplicationProtocol != 2:
		return fmt.Errorf("ReplicationProtocol must be 1 or 2, got %d", o.ReplicationProtocol)
	}
	return nil
}

// offIsZero spells a setting that is Off as the code reads it, zero.
func offIsZero[T int | int64](v T) T {
	if v < 0 {
		return 0
	}
	return v
}

// limits are the space's limits these options describe: every default filled
// in, and every setting turned off spelled as the space spells it, zero.
func (o Options) limits() data_structure.Limits {
	o = o.WithDefaults()
	return data_structure.Limits{
		Eviction:        o.Eviction,
		MaxKeys:         o.MaxKeys,
		MaxMemory:       o.MaxMemory,
		EvictionSamples: o.EvictionSamples,
		LFULogFactor:    offIsZero(o.LFULogFactor),
		LFUDecayPeriod:  offIsZero(o.LFUDecayPeriod),
		LCSMaxCells:     uint64(offIsZero(o.LCSMaxCells)),
	}
}

// settings are an engine's options as the code that is not the space's reads
// them, resolved once, when the options are given: every default filled in,
// and every setting that is off spelled as zero. The keyspace's limits are
// the space's, and the replication role is the engine's role.
type settings struct {
	// Active expiry: the keys one round examines, none being no active
	// expiry; the share, in percent, that earns another round; and the most
	// rounds in a cycle.
	expireSamples, expirePercent, expireRounds int
	// The log: how often it is synced, whether it appends on a worker, and
	// the growth, in percent, none being never, and the size an automatic
	// rewrite waits for.
	fsync             FsyncPolicy
	asyncAppend       bool
	rewritePercentage int
	rewriteMinSize    int64
}

// settings are the settings these options describe.
func (o Options) settings() settings {
	o = o.WithDefaults()
	return settings{
		expireSamples:     offIsZero(o.ActiveExpireSamples),
		expirePercent:     o.ActiveExpirePercent,
		expireRounds:      o.ActiveExpireRounds,
		fsync:             o.Fsync,
		asyncAppend:       o.AsyncAppend,
		rewritePercentage: offIsZero(o.AutoRewritePercentage),
		rewriteMinSize:    offIsZero(o.AutoRewriteMinSize),
	}
}

// role is the replication role these options describe.
func (o Options) role() replicationRole {
	o = o.WithDefaults()
	return replicationRole{ReplicaOf: o.ReplicaOf, Feed: o.ReplicationFeed, Protocol: o.ReplicationProtocol}
}

// configure holds e to o from now on: its space's limits, its settings and
// its role are o's, and what e reports as its options is o, as given.
// Nothing is changed when o is refused.
func (e *Engine) configure(o Options) error {
	if err := o.validate(); err != nil {
		return err
	}
	e.options = o
	e.space.SetLimits(o.limits())
	e.settings = o.settings()
	e.role = o.role()
	return nil
}

// Configuration is e's options, as they were given to it.
func (e *Engine) Configuration() Options { return e.options }

// Limits are the limits e's space is held to: its options, resolved.
func (e *Engine) Limits() data_structure.Limits { return e.space.Limits() }
