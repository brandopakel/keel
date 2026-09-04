package data_structure

import "math/rand"

// Set is an unordered collection of distinct strings.
//
// Members live in two places: a map from member to its position, which answers
// membership in one step, and a slice of the members in no particular order,
// which is what makes choosing one at random O(1) - pick an index. Redis's
// dict does the same job with random bucket probing; a Go map gives nothing
// like that, and ranging over it to pick a member costs a walk of the set.
type Set struct {
	index map[string]int
	order []string
	// memberBytes is the total length of the members held, maintained as they
	// come and go so MemUsage stays O(1). Summing on demand would make a
	// memory-budget check proportional to the size of the set, and that check
	// runs on every write.
	memberBytes uint64
}

func NewSet() *Set {
	return &Set{index: make(map[string]int)}
}

// Add puts members in the set and reports how many were not already there.
func (s *Set) Add(members ...string) int {
	added := 0
	for _, m := range members {
		if _, present := s.index[m]; present {
			continue
		}
		s.index[m] = len(s.order)
		s.order = append(s.order, m)
		s.memberBytes += uint64(len(m))
		added++
	}
	return added
}

// Remove takes members out and reports how many were there to take.
//
// A removal moves the last member into the hole, so the slice stays dense and
// the operation stays O(1) whatever the position.
func (s *Set) Remove(members ...string) int {
	removed := 0
	for _, m := range members {
		i, present := s.index[m]
		if !present {
			continue
		}
		last := len(s.order) - 1
		if i != last {
			s.order[i] = s.order[last]
			s.index[s.order[i]] = i
		}
		s.order[last] = ""
		s.order = s.order[:last]
		delete(s.index, m)
		s.memberBytes -= uint64(len(m))
		removed++
	}
	s.shrink()
	return removed
}

// shrink gives back the slice's spare capacity once most of it is spare, so a
// set that grew and then emptied does not hold its high-water mark.
func (s *Set) shrink() {
	if cap(s.order) > 64 && len(s.order)*4 < cap(s.order) {
		s.order = append(make([]string, 0, len(s.order)), s.order...)
	}
}

// Len is the number of members.
func (s *Set) Len() int { return len(s.order) }

// Contains reports whether member is in the set.
func (s *Set) Contains(member string) bool {
	_, present := s.index[member]
	return present
}

// Members lists every member, in no particular order.
func (s *Set) Members() []string {
	out := make([]string, len(s.order))
	copy(out, s.order)
	return out
}

// swap exchanges two positions and keeps the index pointing at them.
func (s *Set) swap(i, j int) {
	if i == j {
		return
	}
	s.order[i], s.order[j] = s.order[j], s.order[i]
	s.index[s.order[i]] = i
	s.index[s.order[j]] = j
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
// It is a partial Fisher-Yates shuffle of the member slice itself: each step
// swaps a member not yet drawn into the next position, so it costs O(count)
// however large the set, and terminates in exactly count steps however close
// count is to the size of the set. The version this replaced picked indexes at
// random and retried on a collision, which spun forever once count reached the
// size of the set, and the one after it copied every member before choosing.
func (s *Set) RandomMembers(count int) []string {
	if count <= 0 || len(s.order) == 0 {
		return nil
	}
	if count > len(s.order) {
		count = len(s.order)
	}
	for i := 0; i < count; i++ {
		s.swap(i, i+rand.Intn(len(s.order)-i))
	}
	out := make([]string, count)
	copy(out, s.order[:count])
	return out
}

// RandomMembersWithRepeats returns exactly count members drawn independently,
// so the same member can appear more than once. It is what SRANDMEMBER with a
// negative count asks for. An empty set yields nothing.
func (s *Set) RandomMembersWithRepeats(count int) []string {
	if count <= 0 || len(s.order) == 0 {
		return nil
	}
	out := make([]string, count)
	for i := range out {
		out[i] = s.order[rand.Intn(len(s.order))]
	}
	return out
}

// MemUsage estimates the bytes held, in O(1). The per-member overhead is
// measured rather than derived; see memory.go for the figure and how it was
// taken.
func (s *Set) MemUsage() uint64 {
	return setBaseBytes + uint64(len(s.order))*setMemberOverhead + s.memberBytes
}
