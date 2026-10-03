package data_structure

import "github.com/brandopakel/keel/internal/config"

// Space is what a set of stores holds in common: the registry eviction draws
// from, the clock and candidate pool it ranks keys with, the hook that hears
// about removals no client asked for, and the limits the stores are held to.
//
// All of it used to be package variables, which made the keyspace a singleton:
// one per process, and no two tests able to run side by side. The embedding
// plan (docs/embedding-plan.md) gives every engine a Space of its own. Until
// engines exist the server lives in DefaultSpace, and the package-level
// functions at the end of this file act on it, so nothing above this package
// has had to change yet.
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
	// clock.
	limits limitRefs

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

	own Limits
}

// Limits are the bounds a space's stores are held to and the policy that
// enforces them. Each is the config setting of the same name, documented
// there.
type Limits struct {
	EvictStrategy  int
	KeyNumberLimit int
	MaxMemory      uint64
	LRUSamples     int
	LFULogFactor   int
	LFUDecayPeriod int
	LCSMaxCells    uint64
}

// evictionSeed is where every space's xorshift generator starts: the value the
// package variable it replaces was initialised to, so the default space draws
// the sequence it always has.
const evictionSeed = 0x2545F4914F6CDD1D

// limitRefs is where a space reads each limit: its own Limits, or, for
// DefaultSpace, the config variable itself. Reading config live is what keeps
// the flags, and the hundred-odd tests that assign config.MaxMemory and the
// rest, working until engine options replace config (plan step 2.5).
//
// A pointer per limit rather than a flag tested on every read, because it was
// measured to cost less. On a bare Dict.Get, against the package variables
// these replace, the pointers cost 1% under LRU and 6% under LFU, which reads
// two more limits per access; a flag cost 8% and 12%.
type limitRefs struct {
	evictStrategy  *int
	keyNumberLimit *int
	maxMemory      *uint64
	lruSamples     *int
	lfuLogFactor   *int
	lfuDecayPeriod *int
	lcsMaxCells    *uint64
}

// NewSpace returns an empty space held to limits. A space has to come from
// here, or be DefaultSpace: the zero value has nowhere to read its limits.
func NewSpace(limits Limits) *Space {
	s := &Space{rng: evictionSeed, own: limits}
	s.limits = limitRefs{
		evictStrategy:  &s.own.EvictStrategy,
		keyNumberLimit: &s.own.KeyNumberLimit,
		maxMemory:      &s.own.MaxMemory,
		lruSamples:     &s.own.LRUSamples,
		lfuLogFactor:   &s.own.LFULogFactor,
		lfuDecayPeriod: &s.own.LFUDecayPeriod,
		lcsMaxCells:    &s.own.LCSMaxCells,
	}
	return s
}

// DefaultSpace is the space the server's stores live in until each engine owns
// one. It reads its limits from config on every use, as this package always
// has.
var DefaultSpace = &Space{rng: evictionSeed, limits: limitRefs{
	evictStrategy:  &config.EvictStrategy,
	keyNumberLimit: &config.KeyNumberLimit,
	maxMemory:      &config.MaxMemory,
	lruSamples:     &config.LRUSamples,
	lfuLogFactor:   &config.LFULogFactor,
	lfuDecayPeriod: &config.LFUDecayPeriod,
	lcsMaxCells:    &config.LCSMaxCells,
}}

// The package-level functions act on DefaultSpace, so callers read as they did
// while the plan moves them onto spaces of their own.

func RegisterKeyspace(ks Keyspace)        { DefaultSpace.RegisterKeyspace(ks) }
func ResetKeyspaces()                     { DefaultSpace.ResetKeyspaces() }
func OwnerOf(key string) (Keyspace, bool) { return DefaultSpace.OwnerOf(key) }
func DeleteAnywhere(key string) bool      { return DefaultSpace.DeleteAnywhere(key) }
func TotalMemUsed() uint64                { return DefaultSpace.TotalMemUsed() }
func TotalKeys() int                      { return DefaultSpace.TotalKeys() }
func EachKeyspace(fn func(Keyspace))      { DefaultSpace.EachKeyspace(fn) }
func Evicted() uint64                     { return DefaultSpace.Evicted() }
func EnforceLimits()                      { DefaultSpace.EnforceLimits() }
func NewKeyspaceWalk() *KeyspaceWalk      { return DefaultSpace.NewKeyspaceWalk() }
func LCSTooLarge(a, b string) bool        { return DefaultSpace.LCSTooLarge(a, b) }
func EachKeyspaceFrom(start int, fn func(Keyspace)) int {
	return DefaultSpace.EachKeyspaceFrom(start, fn)
}
func VisitKeyspacesFrom(start int, fn func(Keyspace) bool) int {
	return DefaultSpace.VisitKeyspacesFrom(start, fn)
}
func ScanKeyspaces(cursor uint64, budget int, keep func(Keyspace, string) bool, dst []string) ([]string, uint64) {
	return DefaultSpace.ScanKeyspaces(cursor, budget, keep, dst)
}
