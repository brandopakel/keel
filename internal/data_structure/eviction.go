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
func (s *Space) poolInsert(c Candidate) {
	for i := range s.pool {
		if s.pool[i].Key == c.Key && s.pool[i].Keyspace == c.Keyspace {
			// Already a candidate. Refresh rather than holding it twice, which
			// would make the second attempt to evict it a guaranteed miss.
			s.pool[i].Score = c.Score
			s.sortPool()
			return
		}
	}

	if len(s.pool) >= evictionPoolSize && c.Score >= s.pool[len(s.pool)-1].Score {
		// Better than everything already held, so it cannot displace anything.
		return
	}

	s.pool = append(s.pool, c)
	s.sortPool()
	if len(s.pool) > evictionPoolSize {
		s.pool = s.pool[:evictionPoolSize]
	}
}

func (s *Space) sortPool() {
	sort.Slice(s.pool, func(i, j int) bool {
		return s.pool[i].Score < s.pool[j].Score
	})
}
