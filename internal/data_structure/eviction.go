package data_structure

import "sort"

// The candidate pool.
//
// Sampling five keys and evicting the worst is mediocre on its own: the four
// not chosen are discarded, and the next pass starts blind. Keeping them, so
// later samples compete against them, lets the pool accumulate genuinely poor
// keys over successive evictions and converges towards the exact policy without
// ever scanning the keyspace. Measured on LRU, it is the difference between 70%
// and 83% hot-key retention at five samples. Redis added the same thing in 3.0.

// evictionPoolSize is how many candidates survive between evictions.
const evictionPoolSize = 16

// poolInsert adds a candidate, keeping the pool ordered worst-first and no
// longer than evictionPoolSize.
func poolInsert(c Candidate) {
	for i := range evictionPool {
		if evictionPool[i].Key == c.Key && evictionPool[i].Space == c.Space {
			// Already a candidate. Refresh rather than holding it twice, which
			// would make the second attempt to evict it a guaranteed miss.
			evictionPool[i].Score = c.Score
			sortPool()
			return
		}
	}

	if len(evictionPool) >= evictionPoolSize && c.Score >= evictionPool[len(evictionPool)-1].Score {
		// Better than everything already held, so it cannot displace anything.
		return
	}

	evictionPool = append(evictionPool, c)
	sortPool()
	if len(evictionPool) > evictionPoolSize {
		evictionPool = evictionPool[:evictionPoolSize]
	}
}

func sortPool() {
	sort.Slice(evictionPool, func(i, j int) bool {
		return evictionPool[i].Score < evictionPool[j].Score
	})
}
