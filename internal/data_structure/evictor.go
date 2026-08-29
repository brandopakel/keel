package data_structure

import (
	"memkv/internal/config"
)

// The eviction machinery, shared by every keyspace.
//
// Keys live in several typed maps - strings, sets, sorted sets, filters,
// sketches - but a memory budget spans all of them, so eviction has to be able
// to choose between a string key and a sorted set. That needs two things held
// in common: one logical clock, so recency and frequency are on the same scale
// wherever a key lives, and one candidate pool, so a sample can compare across
// keyspaces.
//
// The policy itself is in lru.go and lfu.go. What is here is the plumbing: an
// access word whose meaning the policy decides, a registry of keyspaces, and
// the sampling loop.

var (
	// evictionClock advances on every access anywhere in the keyspace.
	evictionClock uint64

	// evictionPool carries the best candidates between evictions.
	evictionPool []Candidate

	evictionRNG uint64 = 0x2545F4914F6CDD1D

	// keyspaces is every store eviction may draw from.
	keyspaces []Keyspace

	evictedCount uint64
)

// Keyspace is what eviction needs from a store, whatever it holds.
type Keyspace interface {
	KeyspaceName() string
	Len() int
	MemUsed() uint64
	// SampleKeys appends up to n randomly chosen candidates.
	SampleKeys(dst []Candidate, n int) []Candidate
	// ScoreOf reports a key's current score and whether it is still present.
	ScoreOf(key string) (uint64, bool)
	Delete(key string) bool
}

// Candidate is one key considered for eviction. Lower scores go first.
type Candidate struct {
	Space Keyspace
	Key   string
	Score uint64
}

// RegisterKeyspace adds a store to the set eviction may draw from.
func RegisterKeyspace(ks Keyspace) { keyspaces = append(keyspaces, ks) }

// ResetKeyspaces clears the registry and the shared state. For tests, which
// build fresh stores and must not inherit the previous test's keyspaces.
func ResetKeyspaces() {
	keyspaces = nil
	evictionPool = nil
	evictionClock = 0
	evictedCount = 0
}

// TotalMemUsed is the estimated bytes held across every registered keyspace.
func TotalMemUsed() uint64 {
	var total uint64
	for _, ks := range keyspaces {
		total += ks.MemUsed()
	}
	return total
}

// TotalKeys counts keys across every registered keyspace.
func TotalKeys() int {
	n := 0
	for _, ks := range keyspaces {
		n += ks.Len()
	}
	return n
}

// Evicted reports how many keys eviction has removed.
func Evicted() uint64 { return evictedCount }

func nextEvictionRand() uint64 {
	evictionRNG ^= evictionRNG << 13
	evictionRNG ^= evictionRNG >> 7
	evictionRNG ^= evictionRNG << 17
	return evictionRNG
}

// NewAccess is the access word a newly created key starts with.
func NewAccess() uint64 {
	if config.EvictStrategy == config.LFU {
		// A new key needs frequency credit or it is, by definition, the least
		// frequently used thing present and is evicted before it can show
		// otherwise.
		return packLFU(evictionClock, lfuInitVal)
	}
	return evictionClock
}

// Touch records an access. What that means is the policy's business: LRU wants
// to know when, LFU how often.
func Touch(access *uint64) {
	evictionClock++
	if config.EvictStrategy == config.LFU {
		touchLFU(access)
		return
	}
	*access = evictionClock
}

// Score ranks an access word. The lowest score is evicted first.
func Score(access uint64) uint64 {
	if config.EvictStrategy == config.LFU {
		return uint64(decayedFreq(access))
	}
	return access
}

// overLimit reports whether either configured bound is exceeded.
func overLimit() bool {
	if TotalKeys() > config.KeyNumberLimit {
		return true
	}
	return config.MaxMemory > 0 && TotalMemUsed() > config.MaxMemory
}

// EnforceLimits evicts until the keyspace is back inside its bounds.
//
// A key-count bound can only ever be exceeded by one per insert, but a single
// large value can exceed a byte budget by any amount, so this loops. It stops
// when nothing can be evicted: a value larger than the whole budget would
// otherwise clear every keyspace and still not fit, and destroying everything
// to fail anyway helps nobody. The write stands, over budget, as it does in
// Redis under an allkeys policy.
func EnforceLimits() {
	for overLimit() {
		if !evictOne() {
			return
		}
	}
}

// evictOne removes a single key.
func evictOne() bool {
	// EvictFirst consults nothing: it takes whatever comes to hand. Falling
	// through to the sampling path would silently turn it into LRU, since the
	// access word a non-LFU policy stores is a clock reading.
	if config.EvictStrategy != config.LRU && config.EvictStrategy != config.LFU {
		return evictArbitrary()
	}

	samplePool()

	for len(evictionPool) > 0 {
		candidate := evictionPool[0]
		evictionPool = evictionPool[1:]

		score, exists := candidate.Space.ScoreOf(candidate.Key)
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
		if candidate.Space.Delete(candidate.Key) {
			evictedCount++
			return true
		}
	}

	// The pool held nothing usable, which happens when every candidate was
	// touched again. Fall back, so that enforcing a limit always progresses.
	return evictArbitrary()
}

// samplePool draws a fresh sample and merges it into the pool.
//
// Samples are spread across keyspaces in proportion to how many keys each
// holds, so a keyspace with a thousand keys is examined more often than one
// with three - without which a large string keyspace could be starved by a
// handful of sketches.
func samplePool() {
	total := TotalKeys()
	if total == 0 {
		return
	}
	want := config.LRUSamples
	if want < 1 {
		want = 1
	}

	for _, ks := range keyspaces {
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
			poolInsert(c)
		}
	}
}

// evictArbitrary removes a key without regard to recency or frequency.
//
// The keyspace is chosen in proportion to how many keys it holds, so this is
// uniform over keys rather than over keyspaces - otherwise a store with three
// sketches would be raided as often as one with a million strings.
func evictArbitrary() bool {
	total := TotalKeys()
	if total == 0 {
		return false
	}

	pick := int(nextEvictionRand() % uint64(total))
	for _, ks := range keyspaces {
		if pick < ks.Len() {
			for _, c := range ks.SampleKeys(nil, 1) {
				if ks.Delete(c.Key) {
					evictedCount++
					return true
				}
			}
			return false
		}
		pick -= ks.Len()
	}
	return false
}
