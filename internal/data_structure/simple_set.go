package data_structure

import "math/rand"

// simpleSet simply use native hashmap to store keys
type simpleSet struct {
	key  string
	dict map[string]struct{}
	// memberBytes is the total length of the members held, maintained as they
	// come and go so MemUsage stays O(1). Summing on demand would make a
	// memory-budget check proportional to the size of the set, and that check
	// runs on every write.
	memberBytes uint64
}

func newSimpleSet(key string) Set {
	return &simpleSet{
		key:  key,
		dict: make(map[string]struct{}),
	}
}

func (s *simpleSet) Add(members ...string) int {
	added := 0
	for _, m := range members {
		if _, exist := s.dict[m]; !exist {
			s.dict[m] = struct{}{}
			s.memberBytes += uint64(len(m))
			added++
		}
	}
	return added
}

func (s *simpleSet) Rem(members ...string) int {
	removed := 0
	for _, m := range members {
		if _, exist := s.dict[m]; exist {
			delete(s.dict, m)
			s.memberBytes -= uint64(len(m))
			removed++
		}
	}
	return removed
}

func (s *simpleSet) Size() int {
	return len(s.dict)
}

func (s *simpleSet) IsMember(member string) int {
	_, exist := s.dict[member]
	if exist {
		return 1
	}
	return 0
}

func (s *simpleSet) MIsMember(members ...string) []int {
	res := make([]int, len(members))
	for i, m := range members {
		res[i] = s.IsMember(m)
	}
	return res
}

func (s *simpleSet) Members() []string {
	m := make([]string, 0, len(s.dict))
	for k, _ := range s.dict {
		m = append(m, k)
	}
	return m
}

func (s *simpleSet) Pop(count int) []string {
	randKeys := s.Rand(count)
	for _, k := range randKeys {
		delete(s.dict, k)
		s.memberBytes -= uint64(len(k))
	}
	return randKeys
}

// Rand returns up to count members chosen uniformly at random, without
// repeats. Fewer than count come back when the set is smaller than that, and
// none at all when it is empty or count is not positive.
//
// It replaces a version that picked indexes at random and retried on a
// collision, which had three ways to take the whole server down, all of them
// reachable from a client:
//
//	count == 0        the caller indexed [0] into an empty result and panicked,
//	                  which is what plain SPOP and SRAND both did
//	count > Size()    no further index is ever new, so the retry loop spun
//	                  forever - one command, and the event loop never returns
//	Size() == 0       rand.Intn(0) panics
//
// A partial Fisher-Yates shuffle has none of those shapes. It draws each member
// from what has not been drawn yet, so it terminates in exactly count steps
// however close count is to the size of the set, where retrying grew without
// bound as the two converged.
func (s *simpleSet) Rand(count int) []string {
	if count <= 0 || s.Size() == 0 {
		return nil
	}
	if count > s.Size() {
		count = s.Size()
	}

	members := make([]string, 0, s.Size())
	for k := range s.dict {
		members = append(members, k)
	}
	for i := 0; i < count; i++ {
		j := i + rand.Intn(len(members)-i)
		members[i], members[j] = members[j], members[i]
	}
	return members[:count]
}

// MemUsage estimates the bytes held, in O(1).
//
// Per-member overhead is measured rather than derived: adding 200,000 members
// to a map[string]struct{} and reading HeapAlloc either side gives 59 bytes per
// 20-byte member, so 39 bytes of map slot and string header on top of the
// member itself.
func (s *simpleSet) MemUsage() uint64 {
	return setBaseBytes + uint64(len(s.dict))*setMemberOverhead + s.memberBytes
}
