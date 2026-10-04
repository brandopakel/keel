package data_structure

import (
	"github.com/brandopakel/keel/internal/config"
)

// The eviction machinery, shared by every keyspace in a Space.
//
// Keys live in several typed maps - strings, sets, sorted sets, filters,
// sketches - but a memory budget spans all of them, so eviction has to be able
// to choose between a string key and a sorted set. That needs two things held
// in common: one logical clock, so recency and frequency are on the same scale
// wherever a key lives, and one candidate pool, so a sample can compare across
// keyspaces. Both belong to the Space (space.go), as does the registry.
//
// The policy itself is in lru.go and lfu.go. What is here is the plumbing: an
// access word whose meaning the policy decides, a registry of keyspaces, and
// the sampling loop.

// Keyspace is what eviction needs from a store, whatever it holds.
type Keyspace interface {
	KeyspaceName() string
	GetExpiry(string) (uint64, bool)
	SetExpiryAt(string, uint64)
	ClearExpiry(string) bool
	KeysWithExpiry() int
	ActiveExpire(int) (int, int)
	EntryBytes(string) (uint64, bool)
	Len() int
	MemUsed() uint64
	// Has reports whether the key is present, without counting as a use of it.
	Has(key string) bool
	// Keys lists every key held. Expired keys may be included, so a caller
	// showing them to a client has to filter through Has.
	Keys() []string
	// Scan appends the keys keep accepts, from a bounded part of the keyspace,
	// and returns how many it examined and the cursor to resume from - zero
	// once there is nothing left. budget bounds keys examined, not returned.
	Scan(cursor uint64, budget int, keep func(string) bool, dst []string) ([]string, int, uint64)
	ScanEnd() uint64
	ScanUntil(cursor, end uint64, budget int, keep func(string) bool, dst []string) ([]string, int, uint64)
	// SampleKeys appends up to n randomly chosen candidates.
	SampleKeys(dst []Candidate, n int) []Candidate
	// ScoreOf reports a key's current score and whether it is still present.
	ScoreOf(key string) (uint64, bool)
	Delete(key string) bool
}

// Candidate is one key considered for eviction. Lower scores go first.
type Candidate struct {
	Keyspace Keyspace
	Key      string
	Score    uint64
}

func (s *Space) noteRemoval(ks Keyspace, key string) {
	if s.OnRemove != nil {
		s.OnRemove(ks.KeyspaceName(), key)
	}
}

// RegisterKeyspace adds a store to the set eviction may draw from. The store
// has to have been built for this space, since that is the space its writes
// enforce limits in.
func (s *Space) RegisterKeyspace(ks Keyspace) { s.keyspaces = append(s.keyspaces, ks) }

// ResetKeyspaces clears the registry and the shared state. For tests, which
// build fresh stores and must not inherit the previous test's keyspaces.
//
// The generator is not reset, and never was: resetting it would change which
// keys random eviction picks after every ResetStores.
func (s *Space) ResetKeyspaces() {
	s.keyspaces = nil
	s.version++
	s.pool = nil
	s.clock = 0
	s.evicted = 0
}

// OwnerOf reports which keyspace holds a key.
//
// Each type has its own map, so a name was only ever unique within a type: SET
// k v and SADD k m both succeeded, both answered, and DEL k removed the string
// and left the set behind. Nothing arbitrated between the stores because
// nothing knew about all of them at once. This does.
//
// It asks each keyspace in turn rather than keeping a directory of names, and
// the reason is memory. A map from name to owner is a second map entry and a
// second copy of every key - measured at roughly 55 bytes plus the key on top
// of the 100 a key already costs, which showed up immediately as the memory
// estimate falling to 70% of real heap. Per-key memory is the thing this server
// is built around; spending that much of it to answer a question eight map
// lookups can answer would be the wrong trade. The stores are consulted
// strings-first, since most keys are strings and the scan stops at the owner.
func (s *Space) OwnerOf(key string) (Keyspace, bool) {
	for _, ks := range s.keyspaces {
		if ks.Has(key) {
			return ks, true
		}
	}
	return nil, false
}

// DeleteAnywhere removes a key from whichever keyspace holds it.
func (s *Space) DeleteAnywhere(key string) bool {
	if ks, ok := s.OwnerOf(key); ok {
		return ks.Delete(key)
	}
	return false
}

// TotalMemUsed is the estimated bytes held across every registered keyspace.
func (s *Space) TotalMemUsed() uint64 {
	var total uint64
	for _, ks := range s.keyspaces {
		total += ks.MemUsed()
	}
	return total
}

// EachKeyspace calls fn for every registered keyspace, in registration order -
// strings first, which is the order OwnerOf relies on and the order a caller
// listing keys will report them in.
//
// The registry itself stays unexported. Handing out the slice would let a
// caller hold it across a ResetStores and go on writing to keyspaces the server
// has thrown away.
func (s *Space) EachKeyspace(fn func(Keyspace)) {
	for _, ks := range s.keyspaces {
		fn(ks)
	}
}

// TotalKeys counts keys across every registered keyspace.
func (s *Space) TotalKeys() int {
	n := 0
	for _, ks := range s.keyspaces {
		n += ks.Len()
	}
	return n
}

// Evicted reports how many keys eviction has removed.
func (s *Space) Evicted() uint64 { return s.evicted }

func (s *Space) nextRand() uint64 {
	s.rng ^= s.rng << 13
	s.rng ^= s.rng >> 7
	s.rng ^= s.rng << 17
	return s.rng
}

// NewAccess is the access word a newly created key starts with.
func (s *Space) NewAccess() uint64 {
	if *s.limits.evictStrategy == config.LFU {
		// A new key needs frequency credit or it is, by definition, the least
		// frequently used thing present and is evicted before it can show
		// otherwise.
		return packLFU(s.clock, lfuInitVal)
	}
	return s.clock
}

// Touch records an access. What that means is the policy's business: LRU wants
// to know when, LFU how often.
func (s *Space) Touch(access *uint64) {
	s.clock++
	if *s.limits.evictStrategy == config.LFU {
		s.touchLFU(access)
		return
	}
	*access = s.clock
}

// Score ranks an access word. The lowest score is evicted first.
func (s *Space) Score(access uint64) uint64 {
	if *s.limits.evictStrategy == config.LFU {
		return uint64(s.decayedFreq(access))
	}
	return access
}

// overLimit reports whether either configured bound is exceeded.
func (s *Space) overLimit() bool {
	if s.TotalKeys() > *s.limits.keyNumberLimit {
		return true
	}
	maxMemory := *s.limits.maxMemory
	return maxMemory > 0 && s.TotalMemUsed() > maxMemory
}

// EnforceLimits evicts until the keyspace is back inside its bounds.
//
// A key-count bound can only ever be exceeded by one per insert, but a single
// large value can exceed a byte budget by any amount, so this loops. It stops
// when nothing can be evicted: a value larger than the whole budget would
// otherwise clear every keyspace and still not fit, and destroying everything
// to fail anyway helps nobody. The write stands, over budget, as it does in
// Redis under an allkeys policy.
func (s *Space) EnforceLimits() {
	if s.SuspendEviction {
		return
	}
	for s.overLimit() {
		if !s.evictOne() {
			return
		}
	}
}

// evictOne removes a single key.
func (s *Space) evictOne() bool {
	// EvictFirst consults nothing: it takes whatever comes to hand. Falling
	// through to the sampling path would silently turn it into LRU, since the
	// access word a non-LFU policy stores is a clock reading.
	if strategy := *s.limits.evictStrategy; strategy != config.LRU && strategy != config.LFU {
		return s.evictArbitrary()
	}

	s.samplePool()

	for len(s.pool) > 0 {
		candidate := s.pool[0]
		s.pool = s.pool[1:]

		score, exists := candidate.Keyspace.ScoreOf(candidate.Key)
		if !exists {
			continue // deleted or expired since it was sampled
		}
		if score > candidate.Score {
			// Improved since sampling - read again under LRU, accessed again
			// under LFU - so it is no longer a good candidate. Only a rise
			// disqualifies it: a fallen score, which happens on every LFU
			// decay, makes it a better candidate than when it was pooled.
			continue
		}
		if candidate.Keyspace.Delete(candidate.Key) {
			s.evicted++
			s.noteRemoval(candidate.Keyspace, candidate.Key)
			return true
		}
	}

	// The pool held nothing usable, which happens when every candidate was
	// touched again. Fall back, so that enforcing a limit always progresses.
	return s.evictArbitrary()
}

// samplePool draws a fresh sample and merges it into the pool.
//
// Samples are spread across keyspaces in proportion to how many keys each
// holds, so a keyspace with a thousand keys is examined more often than one
// with three - without which a large string keyspace could be starved by a
// handful of sketches.
func (s *Space) samplePool() {
	total := s.TotalKeys()
	if total == 0 {
		return
	}
	want := *s.limits.lruSamples
	if want < 1 {
		want = 1
	}

	for _, ks := range s.keyspaces {
		n := ks.Len()
		if n == 0 {
			continue
		}
		share := want * n / total
		if share == 0 {
			// Round up for small keyspaces so they are never invisible: a
			// single 12KB sketch may be the best thing to evict.
			share = 1
		}
		for _, c := range ks.SampleKeys(nil, share) {
			s.poolInsert(c)
		}
	}
}

// evictArbitrary removes a key without regard to recency or frequency.
//
// The keyspace is chosen in proportion to how many keys it holds, so this is
// uniform over keys rather than over keyspaces - otherwise a store with three
// sketches would be raided as often as one with a million strings.
func (s *Space) evictArbitrary() bool {
	total := s.TotalKeys()
	if total == 0 {
		return false
	}

	pick := int(s.nextRand() % uint64(total))
	for _, ks := range s.keyspaces {
		if pick < ks.Len() {
			for _, c := range ks.SampleKeys(nil, 1) {
				if ks.Delete(c.Key) {
					s.evicted++
					s.noteRemoval(ks, c.Key)
					return true
				}
			}
			return false
		}
		pick -= ks.Len()
	}
	return false
}

// EachKeyspaceFrom visits every store once, rotating the first sampled store.
func (s *Space) EachKeyspaceFrom(start int, fn func(Keyspace)) int {
	if len(s.keyspaces) == 0 {
		return 0
	}
	for i := 0; i < len(s.keyspaces); i++ {
		fn(s.keyspaces[(start+i)%len(s.keyspaces)])
	}
	return (start + 1) % len(s.keyspaces)
}

// VisitKeyspacesFrom stops after the first callback returning false. The cursor
// resumes after the last visited store; a full pass rotates the starting store.
func (s *Space) VisitKeyspacesFrom(start int, fn func(Keyspace) bool) int {
	if len(s.keyspaces) == 0 {
		return 0
	}
	start %= len(s.keyspaces)
	for i := 0; i < len(s.keyspaces); i++ {
		index := (start + i) % len(s.keyspaces)
		if !fn(s.keyspaces[index]) {
			return (index + 1) % len(s.keyspaces)
		}
	}
	return (start + 1) % len(s.keyspaces)
}
