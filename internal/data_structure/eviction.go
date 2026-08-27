package data_structure

import (
	"sort"

	"memkv/internal/config"
)

// Sampled eviction, shared by the approximate LRU and LFU policies.
//
// Neither policy can afford its exact form. True LRU needs every key ordered by
// access time; true LFU needs them ordered by frequency. Both mean a structure
// threaded through the whole keyspace, updated on every read. Instead: score a
// small random sample and evict the worst of it.
//
// The policies differ only in what a score means. Both evict the lowest score,
// so LRU scores a key by when it was last read and LFU by how often, and
// everything below is common.

// evictionPoolSize is how many candidates survive between evictions.
const evictionPoolSize = 16

type poolEntry struct {
	key string
	// score at the time of sampling, so a key that has improved since can be
	// recognised and passed over.
	score uint64
}

// evictBySampling removes one key: the lowest-scoring of a random sample,
// pooled with the best candidates left over from previous passes.
//
// Keeping the pool is what lifts this from mediocre to close to exact. A pass
// that samples five keys and evicts one would otherwise throw away four
// perfectly good candidates; letting them compete against the next sample means
// the pool accumulates genuinely poor keys over time. Measured on LRU, it is
// the difference between 70% and 83% hot-key retention at five samples.
func (d *Dict) evictBySampling(scoreOf func(*Obj) uint64) {
	d.samplePool(scoreOf)

	for len(d.pool) > 0 {
		candidate := d.pool[0]
		d.pool = d.pool[1:]

		obj, exists := d.dictStore[candidate.key]
		if !exists {
			// Deleted or expired since it was sampled.
			continue
		}
		if scoreOf(obj) > candidate.score {
			// It has improved since being sampled - read again under LRU, or
			// accessed again under LFU - so it is no longer a good candidate.
			//
			// Only a rise disqualifies it. A score that has fallen makes it a
			// better candidate than when it was pooled, which happens under LFU
			// whenever decay is applied, and skipping those would pass over
			// exactly the keys the policy is looking for.
			continue
		}

		d.Del(candidate.key)
		d.evicted++
		return
	}

	// Nothing in the pool was usable. Fall back, so that Put always makes room.
	d.evictFirst()
}

// samplePool draws a fresh random sample and merges it into the pool.
//
// The sample comes from ranging over the map, which Go deliberately starts at a
// random bucket. Consecutive keys are therefore neighbours rather than
// independent draws - Redis's own dictGetSomeKeys has the same property - but
// the starting point moves every time, which is what the sampling needs.
func (d *Dict) samplePool(scoreOf func(*Obj) uint64) {
	taken := 0
	for key, obj := range d.dictStore {
		d.poolInsert(key, scoreOf(obj))
		if taken++; taken >= config.LRUSamples {
			break
		}
	}
}

// poolInsert adds a candidate, keeping the pool ordered worst-first and no
// longer than evictionPoolSize.
func (d *Dict) poolInsert(key string, score uint64) {
	for i := range d.pool {
		if d.pool[i].key == key {
			// Already a candidate. Refresh it rather than letting the pool hold
			// two entries for one key, which would make the second attempt to
			// evict it a guaranteed miss.
			d.pool[i].score = score
			d.sortPool()
			return
		}
	}

	if len(d.pool) >= evictionPoolSize && score >= d.pool[len(d.pool)-1].score {
		// Better than every candidate already held, so it cannot displace one.
		return
	}

	d.pool = append(d.pool, poolEntry{key: key, score: score})
	d.sortPool()
	if len(d.pool) > evictionPoolSize {
		d.pool = d.pool[:evictionPoolSize]
	}
}

func (d *Dict) sortPool() {
	sort.Slice(d.pool, func(i, j int) bool { return d.pool[i].score < d.pool[j].score })
}
