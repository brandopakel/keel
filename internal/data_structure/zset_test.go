package data_structure

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestZSetAddReportsWhatItDid(t *testing.T) {
	zs := CreateZSet()
	assert.Equal(t, ZAddAdded, zs.Add(1, "a", 0))
	assert.Equal(t, ZAddNop, zs.Add(1, "a", 0), "same score is no change")
	assert.Equal(t, ZAddUpdated, zs.Add(2, "a", 0))
	assert.Equal(t, 1, zs.Len())

	score, ok := zs.Score("a")
	assert.True(t, ok)
	assert.Equal(t, 2.0, score)
	_, ok = zs.Score("missing")
	assert.False(t, ok)
}

func TestZSetAddFlags(t *testing.T) {
	zs := CreateZSet()
	zs.Add(1, "a", 0)

	assert.Equal(t, ZAddNop, zs.Add(5, "a", ZAddNX), "NX leaves an existing member alone")
	assert.Equal(t, ZAddAdded, zs.Add(5, "b", ZAddNX), "NX still adds a new one")

	assert.Equal(t, ZAddNop, zs.Add(7, "c", ZAddXX), "XX does not add")
	assert.Equal(t, ZAddUpdated, zs.Add(7, "a", ZAddXX), "XX does rescore")
	assert.Equal(t, 2, zs.Len())

	score, _ := zs.Score("a")
	assert.Equal(t, 7.0, score)
}

func TestZSetRankFollowsScoreOrder(t *testing.T) {
	zs := CreateZSet()
	zs.Add(30, "c", 0)
	zs.Add(10, "a", 0)
	zs.Add(20, "b", 0)
	zs.Add(20, "bb", 0) // same score as b, sorts after it by name

	for i, m := range []string{"a", "b", "bb", "c"} {
		rank, ok := zs.Rank(m, false)
		assert.True(t, ok)
		assert.EqualValues(t, i, rank, "%s forward", m)
		rank, ok = zs.Rank(m, true)
		assert.True(t, ok)
		assert.EqualValues(t, 3-i, rank, "%s reverse", m)
	}
	_, ok := zs.Rank("nobody", false)
	assert.False(t, ok)

	// Rescoring moves a member and every rank follows.
	zs.Add(5, "c", 0)
	rank, _ := zs.Rank("c", false)
	assert.EqualValues(t, 0, rank)
	rank, _ = zs.Rank("a", false)
	assert.EqualValues(t, 1, rank)
}

func TestZSetRemove(t *testing.T) {
	zs := CreateZSet()
	zs.Add(1, "a", 0)
	zs.Add(2, "b", 0)

	assert.True(t, zs.Remove("a"))
	assert.False(t, zs.Remove("a"), "a second removal finds nothing")
	assert.False(t, zs.Remove("never"))
	assert.Equal(t, 1, zs.Len())

	rank, ok := zs.Rank("b", false)
	assert.True(t, ok)
	assert.EqualValues(t, 0, rank, "b moves up once a is gone")
}

func TestZSetEntriesPairMembersWithScores(t *testing.T) {
	zs := CreateZSet()
	want := map[string]float64{"x": 1.5, "y": -2, "z": 0}
	for m, s := range want {
		zs.Add(s, m, 0)
	}
	members, scores := zs.Entries()
	assert.Len(t, members, 3)
	got := map[string]float64{}
	for i, m := range members {
		got[m] = scores[i]
	}
	assert.Equal(t, want, got)
}

func TestZSetMemUsageFollowsMembers(t *testing.T) {
	zs := CreateZSet()
	empty := zs.MemUsage()
	zs.Add(1, "twenty-byte-member!!", 0)
	one := zs.MemUsage()
	assert.Greater(t, one, empty)
	assert.Equal(t, empty+zsetMemberOverhead+20, one)

	zs.Add(2, "twenty-byte-member!!", 0)
	assert.Equal(t, one, zs.MemUsage(), "rescoring costs nothing")
	zs.Remove("twenty-byte-member!!")
	assert.Equal(t, empty, zs.MemUsage(), "what was charged is credited back")
}

func TestZSetOrderMatchesSortOfEntries(t *testing.T) {
	zs := CreateZSet()
	in := []scored{{3, "c"}, {1, "a"}, {2, "b"}, {2, "aa"}, {-1, "neg"}}
	for _, e := range in {
		zs.Add(e.score, e.ele, 0)
	}
	want := sortedEntries(in)
	sort.Slice(want, func(i, j int) bool {
		if want[i].score != want[j].score {
			return want[i].score < want[j].score
		}
		return want[i].ele < want[j].ele
	})
	for i, e := range want {
		rank, ok := zs.Rank(e.ele, false)
		assert.True(t, ok)
		assert.EqualValues(t, i, rank)
	}
}
