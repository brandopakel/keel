package data_structure

import (
	"sort"

	"memkv/internal/config"
)

// Approximate LRU eviction.
//
// True LRU needs every key ordered by access time, which means a linked list
// threaded through the whole keyspace and pointer updates on every read. That
// costs memory per key and turns a lookup into a write. Redis does not pay it,
// and neither does this: instead of finding the least recently used key, sample
// a handful at random and evict the oldest of those.
//
// With five samples the key evicted is very likely to be old, but is not
// necessarily the oldest - hence approximate. The refinement that closes most
// of that gap is the pool below: rather than discarding the four candidates it
// did not evict, each pass keeps the best ones and lets later samples compete
// against them. Over successive evictions the pool accumulates genuinely old
// keys, and the choice converges towards true LRU without ever scanning the
// keyspace. Redis added this in 3.0 for the same reason.

// evictionPoolSize is how many candidates survive between evictions.
const evictionPoolSize = 16

type poolEntry struct {
	key string
	// lastAccess is recorded when the candidate is sampled, so a key that is
	// read again before it is evicted can be recognised as stale.
	lastAccess uint64
}

// evictApproxLRU removes one key, chosen as the oldest of a random sample
// pooled with the best candidates from previous passes.
func (d *Dict) evictApproxLRU() {
	d.samplePool()

	for len(d.pool) > 0 {
		candidate := d.pool[0]
		d.pool = d.pool[1:]

		obj, exists := d.dictStore[candidate.key]
		switch {
		case !exists:
			// Deleted or expired since it was sampled.
			continue
		case obj.LastAccess != candidate.lastAccess:
			// Read since it was sampled, so it is no longer a good candidate.
			// Dropping it is right: it will be resampled if it goes cold again.
			continue
		}

		d.Del(candidate.key)
		return
	}

	// The pool held nothing usable, which happens when everything in it was
	// touched again. Fall back so that Put always makes room.
	d.evictFirst()
}

// samplePool draws a fresh random sample and merges it into the pool.
//
// The sample comes from ranging over the map, which Go deliberately starts at a
// random bucket. Consecutive keys are therefore neighbours rather than
// independent draws - Redis's own dictGetSomeKeys has the same property - but
// the starting point moves every time, which is what the sampling needs.
func (d *Dict) samplePool() {
	taken := 0
	for key, obj := range d.dictStore {
		d.poolInsert(key, obj.LastAccess)
		if taken++; taken >= config.LRUSamples {
			break
		}
	}
}

// poolInsert adds a candidate, keeping the pool ordered oldest-access-first and
// no longer than evictionPoolSize.
func (d *Dict) poolInsert(key string, lastAccess uint64) {
	for i := range d.pool {
		if d.pool[i].key == key {
			// Already a candidate. Refresh it rather than letting the pool hold
			// two entries for one key, which would make the second attempt to
			// evict it a guaranteed miss.
			d.pool[i].lastAccess = lastAccess
			d.sortPool()
			return
		}
	}

	if len(d.pool) >= evictionPoolSize && lastAccess >= d.pool[len(d.pool)-1].lastAccess {
		// Newer than every candidate already held, so it cannot displace one.
		return
	}

	d.pool = append(d.pool, poolEntry{key: key, lastAccess: lastAccess})
	d.sortPool()
	if len(d.pool) > evictionPoolSize {
		d.pool = d.pool[:evictionPoolSize]
	}
}

func (d *Dict) sortPool() {
	sort.Slice(d.pool, func(i, j int) bool {
		return d.pool[i].lastAccess < d.pool[j].lastAccess
	})
}
