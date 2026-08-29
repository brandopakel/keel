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

// TODO: optimize
func (s *simpleSet) Rand(count int) []string {
	temp := make([]string, 0, s.Size())
	for k := range s.dict {
		temp = append(temp, k)
	}

	res := make([]string, count)
	r := make(map[int]struct{})
	for i := 0; i < count; i++ {
		for {
			picked := rand.Intn(s.Size())
			if _, ok := r[picked]; !ok {
				res[i] = temp[picked]
				r[picked] = struct{}{}
				break
			}
		}
	}
	return res
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
