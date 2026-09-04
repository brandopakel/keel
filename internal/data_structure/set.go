package data_structure

import "math/rand"

// Set is an unordered collection of distinct strings.
type Set struct {
	members map[string]struct{}
	// memberBytes is the total length of the members held, maintained as they
	// come and go so MemUsage stays O(1). Summing on demand would make a
	// memory-budget check proportional to the size of the set, and that check
	// runs on every write.
	memberBytes uint64
}

func NewSet() *Set {
	return &Set{members: make(map[string]struct{})}
}

// Add puts members in the set and reports how many were not already there.
func (s *Set) Add(members ...string) int {
	added := 0
	for _, m := range members {
		if _, present := s.members[m]; present {
			continue
		}
		s.members[m] = struct{}{}
		s.memberBytes += uint64(len(m))
		added++
	}
	return added
}

// Remove takes members out and reports how many were there to take.
func (s *Set) Remove(members ...string) int {
	removed := 0
	for _, m := range members {
		if _, present := s.members[m]; !present {
			continue
		}
		delete(s.members, m)
		s.memberBytes -= uint64(len(m))
		removed++
	}
	return removed
}

// Len is the number of members.
func (s *Set) Len() int { return len(s.members) }

// Contains reports whether member is in the set.
func (s *Set) Contains(member string) bool {
	_, present := s.members[member]
	return present
}

// Members lists every member, in no particular order.
func (s *Set) Members() []string {
	out := make([]string, 0, len(s.members))
	for m := range s.members {
		out = append(out, m)
	}
	return out
}

// Pop removes up to count members chosen at random and returns them.
func (s *Set) Pop(count int) []string {
	picked := s.RandomMembers(count)
	s.Remove(picked...)
	return picked
}

// RandomMembers returns up to count members chosen uniformly at random, without
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
func (s *Set) RandomMembers(count int) []string {
	if count <= 0 || len(s.members) == 0 {
		return nil
	}
	if count > len(s.members) {
		count = len(s.members)
	}

	members := s.Members()
	for i := 0; i < count; i++ {
		j := i + rand.Intn(len(members)-i)
		members[i], members[j] = members[j], members[i]
	}
	return members[:count]
}

// RandomMembersWithRepeats returns exactly count members drawn independently,
// so the same member can appear more than once. It is what SRANDMEMBER with a
// negative count asks for. An empty set yields nothing.
func (s *Set) RandomMembersWithRepeats(count int) []string {
	if count <= 0 || len(s.members) == 0 {
		return nil
	}
	members := s.Members()
	out := make([]string, count)
	for i := range out {
		out[i] = members[rand.Intn(len(members))]
	}
	return out
}

// MemUsage estimates the bytes held, in O(1).
//
// Per-member overhead is measured rather than derived: adding 200,000 members
// to a map[string]struct{} and reading HeapAlloc either side gives 59 bytes per
// 20-byte member, so 39 bytes of map slot and string header on top of the
// member itself.
func (s *Set) MemUsage() uint64 {
	return setBaseBytes + uint64(len(s.members))*setMemberOverhead + s.memberBytes
}
