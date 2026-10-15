package data_structure

// Space is what a set of stores holds in common: the registry eviction draws
// from, the clock and candidate pool it ranks keys with, the hook that hears
// about removals no client asked for, and the limits the stores are held to.
//
// All of it used to be package variables, which made the keyspace a singleton:
// one per process, and no two tests able to run side by side. The embedding
// plan (docs/embedding-plan.md) gives every engine a Space of its own, made
// with NewSpace; since step 2.7 no space is the package's.
//
// A Space is not safe for concurrent use. Like the stores in it, it belongs to
// whoever is executing commands: the event loop's thread today, and the holder
// of the engine's lock once there is one.
//
// Each store keeps a pointer to the Space it was built for. That is eight bytes
// per store rather than per key, so the per-key calibration in memory.go does
// not move.
type Space struct {
	// clock advances on every access anywhere in the space.
	clock uint64

	// limits is read on every access, for the policy, so it sits next to the
	// clock. They are held by value, so reading one is a load from the space
	// rather than a load of a pointer and then of what it points at.
	limits Limits
	// noTouch is set while a command of a connection in CLIENT NO-TOUCH mode
	// runs: what it reads is left as it was, unaccessed (Touch).
	noTouch bool
	// touchSlow is set while an access needs more than the clock, under an
	// LFU policy or under no-touch: the one test Touch makes (setTouchSlow).
	touchSlow bool

	// pool carries the best candidates between evictions.
	pool []Candidate

	rng uint64

	// keyspaces is every store eviction may draw from, in registration order.
	keyspaces []Keyspace
	// version changes whenever the registry is reset, so a walk begun over the
	// stores that were thrown away can tell.
	version uint64

	evicted uint64

	// OnRemove is called when a key leaves a keyspace because the server
	// decided so, rather than because a client asked: a TTL falling due, or
	// eviction making room. Nothing here uses it; persistence does.
	//
	// A removal a client asked for needs no hook, because the command that
	// asked is itself what gets recorded. These two have no command behind
	// them, and an append-only log that does not record them replays into a
	// keyspace holding keys the original had already dropped - which under a
	// memory bound then evicts a different set again, so the divergence
	// compounds rather than settles.
	OnRemove func(keyspace, key string)

	// SuspendEviction stops EnforceLimits from doing anything.
	//
	// Set while an append-only file is being replayed. The log already records
	// every eviction the original run performed, as a DEL, so replay has only
	// to apply those; letting it evict as well means two eviction passes over
	// one sequence of writes. Worse, the second pass chooses independently -
	// the keys it drops are not the keys the DELs then drop - so the keyspace
	// loses roughly twice as many keys as it should and the two runs diverge
	// instead of matching.
	//
	// The bound is enforced once, at the end of the replay, so a log written
	// under a larger limit than the one now configured still lands inside it.
	SuspendEviction bool

	// SuspendExpiry pins the clock every expiry is compared against at zero,
	// so that nothing reads as expired and historical log mutations do not
	// depend on the wall clock at replay.
	SuspendExpiry bool

	// ttlSum and ttlSamples are the time left, in milliseconds, of the keys
	// active expiry found not yet due, which the engine's expiry cycle takes
	// for INFO's avg_ttl (TakeTTLSamples). Only that cycle reads them, so they
	// go last, after everything an access reads.
	ttlSum, ttlSamples uint64
	// expired counts the keys removed because their TTL had passed, by a read
	// that found them so or by active expiry, for INFO's expired_keys
	// (NoteExpired). Only a removal writes it, so it goes last too.
	expired uint64
}

// EvictionPolicy is how a space that is over one of its bounds chooses the
// keys to evict. The zero value is EvictLRU.
//
// A policy is a set of flags, as Redis's maxmemory policies are (server.h),
// so that the test an access makes, whether keys are ranked by frequency, is
// one bit whatever the policy.
type EvictionPolicy int

// What a policy is made of: ranking by frequency rather than recency, taking
// keys at random, choosing among the keys that carry a TTL only, ranking them
// by when they fall due, and evicting nothing.
const (
	evictByFrequency EvictionPolicy = 1 << iota
	evictAtRandom
	evictVolatile
	evictByTTL
	evictNothing
)

// The eviction policies, by Redis's names, which INFO reports.
const (
	// EvictLRU evicts the least recently used key of a sample: allkeys-lru.
	EvictLRU EvictionPolicy = 0
	// EvictLFU evicts the least frequently used key of a sample:
	// allkeys-lfu.
	EvictLFU = evictByFrequency
	// EvictRandom takes whichever key comes to hand, without regard to use:
	// allkeys-random.
	EvictRandom = evictAtRandom
	// EvictVolatileLRU, EvictVolatileLFU and EvictVolatileRandom choose as
	// their namesakes do, among the keys that carry a TTL only:
	// volatile-lru, volatile-lfu and volatile-random.
	EvictVolatileLRU    = evictVolatile
	EvictVolatileLFU    = evictVolatile | evictByFrequency
	EvictVolatileRandom = evictVolatile | evictAtRandom
	// EvictVolatileTTL evicts, of a sample of the keys that carry a TTL, the
	// one due soonest: volatile-ttl.
	EvictVolatileTTL = evictVolatile | evictByTTL
	// EvictNone evicts nothing: noeviction. A space over its bound stays
	// over it, and the engine refuses the commands that would grow it.
	EvictNone = evictNothing
)

// Volatile reports whether p chooses among the keys that carry a TTL only.
func (p EvictionPolicy) Volatile() bool { return p&evictVolatile != 0 }

// Evicts reports whether p evicts anything at all.
func (p EvictionPolicy) Evicts() bool { return p&evictNothing == 0 }

// MayStayOver reports whether a space under p may stay over its bound after
// evicting what it can: under noeviction, or a volatile policy once no key
// carries a TTL. An allkeys policy always brings it back inside.
func (p EvictionPolicy) MayStayOver() bool { return p&(evictVolatile|evictNothing) != 0 }

// Limits are the bounds a space's stores are held to, and the parameters of
// the policy that keeps them inside those bounds. Each is taken as it is
// written: zero means none where a field says so. The engine fills them from
// its options (see core.Options), which is where the defaults are chosen.
type Limits struct {
	// Eviction is the policy a bound evicts by.
	Eviction EvictionPolicy

	// MaxKeys bounds the keyspace by count: once it is reached, a write evicts
	// before it lands. Zero is no bound, as in Redis, where only the memory
	// bound limits the keyspace.
	MaxKeys int

	// MaxMemory bounds the stores in bytes. Zero is no bound.
	//
	// The figure is an estimate rather than a measurement - Go offers no way
	// to ask the allocator what a value cost - so it is a target, not a
	// guarantee. See entryBytes in memory.go.
	MaxMemory uint64

	// EvictionSamples is how many random keys an approximate-LRU or -LFU
	// eviction looks at before choosing one. Redis calls this
	// maxmemory-samples and defaults to 5: scanning every key to find the true
	// least-recently-used one would make eviction O(n) and is the whole reason
	// the policy is approximate. Fewer than one samples one.
	EvictionSamples int

	// LFULogFactor controls how quickly the logarithmic access counter
	// saturates. Higher means a slower rise, so the counter distinguishes
	// larger access counts at the cost of resolution among small ones. Redis
	// calls this lfu-log-factor and defaults to 10. Zero counts every access.
	LFULogFactor int

	// LFUDecayPeriod is how many accesses across the whole space pass before
	// an idle key's counter drops by one. Zero never decays. Redis measures
	// this in minutes of wall clock; measuring it in accesses instead ties
	// forgetting to how busy the cache is rather than to how long the process
	// has been running, and keeps eviction behaviour reproducible.
	//
	// The default is measured rather than guessed, against the two workloads
	// that pull in opposite directions - a scan that should not displace a
	// working set, and a working set that moves and should be followed:
	//
	//	period    scan resistance    stale kept    current kept
	//	  none          99.2%           72.0%          75.4%
	//	100000          99.2%           57.6%          91.8%
	//	 10000          99.2%            1.0%         100.0%
	//	  1000          70.4%            1.4%         100.0%
	//	   100           1.2%              -              -
	//
	// Too short and frequency never accumulates, leaving LFU no better than
	// LRU; too long and the cache fills with keys that were popular once.
	LFUDecayPeriod int

	// LCSMaxCells bounds len(key1)*len(key2) for the LCS command, which is the
	// number of cell comparisons it performs. Zero is no bound.
	//
	// The default is 134217728, which is where Redis stops: its LCS builds an
	// (n+1)(m+1) table of uint32 and refuses once that allocation would exceed
	// proto-max-bulk-len, 512MB by default. Keel does not build the table, so
	// the same figure is reached for an entirely different reason - it is a
	// time budget. Commands run one at a time, so an LCS does not merely take
	// a while, it takes the engine away from every other caller for the
	// duration. Measured at 410 million cells per second on darwin/arm64, the
	// default is about 330ms of stall in the worst case, which is a lot; it is
	// set to match what Redis will answer rather than to be comfortable.
	// Lower it if tail latency matters more than accepting every input Redis
	// accepts.
	LCSMaxCells uint64
}

// DefaultLimits are the limits of a space nobody has set any for: no bound
// on keys or memory, LRU, and Redis's sampling, LFU counter and LCS figures,
// as documented on each field.
func DefaultLimits() Limits {
	return Limits{
		Eviction:        EvictLRU,
		EvictionSamples: 5,
		LFULogFactor:    10,
		LFUDecayPeriod:  10000,
		LCSMaxCells:     134217728,
	}
}

// evictionSeed is where every space's xorshift generator starts: the value the
// package variable it replaces was initialised to, so the default space draws
// the sequence it always has.
const evictionSeed = 0x2545F4914F6CDD1D

// NewSpace returns an empty space held to limits.
func NewSpace(limits Limits) *Space {
	s := &Space{rng: evictionSeed, limits: limits}
	s.setTouchSlow()
	return s
}

// Limits reports the limits the space is held to.
func (s *Space) Limits() Limits { return s.limits }

// SetLimits holds the space to limits from now on. Keys already over a new
// bound are evicted by the next write, as they would be by a lower bound
// configured at startup. A space's policy is meant to be chosen before it
// holds keys: changing it later reinterprets the access words of the keys it
// holds, which LRU and LFU read differently.
func (s *Space) SetLimits(limits Limits) {
	if limits.Eviction != s.limits.Eviction {
		// The pool's candidates were scored by the old policy.
		s.pool = nil
	}
	s.limits = limits
	s.setTouchSlow()
}

// EvictionPolicy is how the space chooses the keys it evicts.
func (s *Space) EvictionPolicy() EvictionPolicy { return s.limits.Eviction }

// MaxKeys is the space's bound on its key count; zero is none.
func (s *Space) MaxKeys() int { return s.limits.MaxKeys }

// MaxMemory is the space's bound on its estimated bytes; zero is none.
func (s *Space) MaxMemory() uint64 { return s.limits.MaxMemory }
