package data_structure

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSetAddAndRemoveCountWhatChanged(t *testing.T) {
	s := NewSet()
	assert.Equal(t, 2, s.Add("a", "b"))
	assert.Equal(t, 1, s.Add("b", "c"), "only c is new")
	assert.Equal(t, 3, s.Len())
	assert.True(t, s.Contains("a"))
	assert.False(t, s.Contains("z"))

	assert.Equal(t, 2, s.Remove("a", "z", "c"))
	assert.Equal(t, 0, s.Remove("a"))
	assert.Equal(t, []string{"b"}, s.Members())
}

func TestSetMembersListsEveryone(t *testing.T) {
	s := NewSet()
	s.Add("x", "y", "z")
	got := s.Members()
	sort.Strings(got)
	assert.Equal(t, []string{"x", "y", "z"}, got)
	assert.Empty(t, NewSet().Members())
}

func TestSetRandomMembersAreDistinctAndBounded(t *testing.T) {
	s := NewSet()
	s.Add("a", "b", "c", "d", "e")

	assert.Nil(t, s.RandomMembers(0), "nothing asked, nothing given")
	assert.Nil(t, s.RandomMembers(-3))
	assert.Nil(t, NewSet().RandomMembers(3), "nothing to give")

	for trial := 0; trial < 50; trial++ {
		picked := s.RandomMembers(3)
		assert.Len(t, picked, 3)
		seen := map[string]bool{}
		for _, m := range picked {
			assert.True(t, s.Contains(m))
			assert.False(t, seen[m], "no repeats")
			seen[m] = true
		}
	}
	all := s.RandomMembers(50)
	assert.Len(t, all, 5, "more than the set holds is the whole set")
	assert.Equal(t, 5, s.Len(), "and nothing was removed")
}

func TestSetRandomMembersCoverTheSet(t *testing.T) {
	s := NewSet()
	s.Add("a", "b", "c", "d")
	seen := map[string]int{}
	for i := 0; i < 2000; i++ {
		seen[s.RandomMembers(1)[0]]++
	}
	for _, m := range []string{"a", "b", "c", "d"} {
		assert.Greater(t, seen[m], 300, "%s should come up about a quarter of the time", m)
	}
}

func TestSetRandomMembersWithRepeats(t *testing.T) {
	s := NewSet()
	s.Add("only")
	assert.Equal(t, []string{"only", "only", "only"}, s.RandomMembersWithRepeats(3),
		"drawing with replacement can repeat, and with one member must")
	assert.Nil(t, s.RandomMembersWithRepeats(0))
	assert.Nil(t, NewSet().RandomMembersWithRepeats(3))

	s.Add("other")
	picked := s.RandomMembersWithRepeats(100)
	assert.Len(t, picked, 100, "exactly the count asked, however small the set")
	for _, m := range picked {
		assert.True(t, s.Contains(m))
	}
}

func TestSetPopRemovesWhatItReturns(t *testing.T) {
	s := NewSet()
	s.Add("a", "b", "c")
	popped := s.Pop(2)
	assert.Len(t, popped, 2)
	assert.Equal(t, 1, s.Len())
	for _, m := range popped {
		assert.False(t, s.Contains(m))
	}
	assert.Len(t, s.Pop(5), 1, "the rest")
	assert.Zero(t, s.Len())
	assert.Nil(t, s.Pop(1), "nothing left")
}

func TestSetMemUsageFollowsMembers(t *testing.T) {
	s := NewSet()
	empty := s.MemUsage()
	s.Add("twenty-byte-member!!")
	assert.Equal(t, empty+setMemberOverhead+20, s.MemUsage())
	s.Add("twenty-byte-member!!")
	assert.Equal(t, empty+setMemberOverhead+20, s.MemUsage(), "a duplicate costs nothing")
	s.Pop(1)
	assert.Equal(t, empty, s.MemUsage(), "what was charged is credited back")
}
