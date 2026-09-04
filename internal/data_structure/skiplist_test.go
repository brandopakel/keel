package data_structure

import (
	"math/rand"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
)

type scored struct {
	score float64
	ele   string
}

// sortedEntries is the order the list must hold: by score, then by member.
func sortedEntries(in []scored) []scored {
	out := append([]scored(nil), in...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score < out[j].score
		}
		return out[i].ele < out[j].ele
	})
	return out
}

func walk(sl *skiplist) []scored {
	var out []scored
	for n := sl.first(); n != nil; n = n.next() {
		out = append(out, scored{n.score, n.ele})
	}
	return out
}

func TestSkiplistOrdersByScoreThenMember(t *testing.T) {
	sl := newSkiplist()
	in := []scored{{3, "c"}, {1, "z"}, {2, "b"}, {2, "a"}, {1, "y"}, {3, "a"}}
	for _, e := range in {
		sl.insert(e.score, e.ele)
	}
	assert.Equal(t, sortedEntries(in), walk(sl))
	assert.EqualValues(t, len(in), sl.length)
	assert.Equal(t, "c", sl.tail.ele, "the tail is the last node in order")
	assert.Nil(t, sl.first().backward, "the first node has nothing behind it")
}

func TestSkiplistRankIsOneBasedPosition(t *testing.T) {
	sl := newSkiplist()
	in := []scored{{10, "a"}, {10, "b"}, {20, "c"}, {40, "d"}, {50, "e"}, {50, "f"}}
	for _, e := range in {
		sl.insert(e.score, e.ele)
	}
	for i, e := range sortedEntries(in) {
		assert.EqualValues(t, i+1, sl.rank(e.score, e.ele), "%v", e)
		assert.Equal(t, e.ele, sl.nodeAtRank(uint64(i+1)).ele)
	}
	assert.Zero(t, sl.rank(10, "nope"), "an absent member has no rank")
	assert.Zero(t, sl.rank(11, "a"), "the right member at the wrong score is absent")
	assert.Nil(t, sl.nodeAtRank(0))
	assert.Nil(t, sl.nodeAtRank(uint64(len(in)+1)))
}

func TestSkiplistRemoveKeepsRanksConsistent(t *testing.T) {
	sl := newSkiplist()
	var in []scored
	for i := 0; i < 200; i++ {
		e := scored{float64(i % 37), string(rune('a'+i%26)) + string(rune('a'+i/26))}
		in = append(in, e)
		sl.insert(e.score, e.ele)
	}

	assert.False(t, sl.remove(1, "not-there"))
	assert.False(t, sl.remove(999, in[0].ele), "the member must match at its own score")

	// Take out every third entry and check what is left still ranks in order.
	var kept []scored
	for i, e := range in {
		if i%3 == 0 {
			assert.True(t, sl.remove(e.score, e.ele))
		} else {
			kept = append(kept, e)
		}
	}
	assert.Equal(t, sortedEntries(kept), walk(sl))
	assert.EqualValues(t, len(kept), sl.length)
	for i, e := range sortedEntries(kept) {
		assert.EqualValues(t, i+1, sl.rank(e.score, e.ele))
	}

	// Empty it, and the header is all that remains.
	for _, e := range kept {
		assert.True(t, sl.remove(e.score, e.ele))
	}
	assert.Zero(t, sl.length)
	assert.Nil(t, sl.first())
	assert.Nil(t, sl.tail)
	assert.Equal(t, 1, sl.level, "levels are shed as the nodes in them go")
}

func TestSkiplistUpdateScore(t *testing.T) {
	sl := newSkiplist()
	for _, e := range []scored{{1, "a"}, {2, "b"}, {3, "c"}, {4, "d"}} {
		sl.insert(e.score, e.ele)
	}

	// Staying between the same neighbours changes the node in place.
	before := sl.nodeAtRank(2)
	after := sl.updateScore(2, "b", 2.5)
	assert.Same(t, before, after)
	assert.Equal(t, []scored{{1, "a"}, {2.5, "b"}, {3, "c"}, {4, "d"}}, walk(sl))

	// Crossing a neighbour moves the node.
	moved := sl.updateScore(2.5, "b", 10)
	assert.NotSame(t, before, moved)
	assert.Equal(t, []scored{{1, "a"}, {3, "c"}, {4, "d"}, {10, "b"}}, walk(sl))
	assert.EqualValues(t, 4, sl.rank(10, "b"))
	assert.EqualValues(t, 4, sl.length)
	assert.Equal(t, "b", sl.tail.ele)

	assert.Panics(t, func() { sl.updateScore(99, "b", 1) }, "the member must be present at the old score")
}

func TestSkiplistRanges(t *testing.T) {
	sl := newSkiplist()
	for _, e := range []scored{{1, "a"}, {2, "b"}, {2, "c"}, {5, "d"}, {8, "e"}} {
		sl.insert(e.score, e.ele)
	}

	closed := rangeSpec{min: 2, max: 5}
	assert.True(t, sl.overlaps(closed))
	assert.Equal(t, "b", sl.firstInRange(closed).ele)
	assert.Equal(t, "d", sl.lastInRange(closed).ele)

	openBelow := rangeSpec{min: 2, max: 5, minEx: true}
	assert.Equal(t, "d", sl.firstInRange(openBelow).ele, "2 is excluded so the next score up is first")
	assert.Equal(t, "d", sl.lastInRange(openBelow).ele)

	openAbove := rangeSpec{min: 2, max: 5, maxEx: true}
	assert.Equal(t, "b", sl.firstInRange(openAbove).ele)
	assert.Equal(t, "c", sl.lastInRange(openAbove).ele, "5 is excluded so the last of the 2s is last")

	// overlaps is only the cheap check against the two ends; the range
	// functions do the exact one, and nothing lies strictly between 2 and 5.
	open := rangeSpec{min: 2, max: 5, minEx: true, maxEx: true}
	assert.True(t, sl.overlaps(open))
	assert.Nil(t, sl.firstInRange(open))
	assert.Nil(t, sl.lastInRange(open))

	assert.Nil(t, sl.firstInRange(rangeSpec{min: 3, max: 4}), "nothing between the scores present")
	assert.False(t, sl.overlaps(rangeSpec{min: 9, max: 100}), "above every score")
	assert.False(t, sl.overlaps(rangeSpec{min: -5, max: 0}), "below every score")
	assert.False(t, sl.overlaps(rangeSpec{min: 5, max: 2}), "inverted")
	assert.False(t, sl.overlaps(rangeSpec{min: 5, max: 5, maxEx: true}), "a point range with an open end holds nothing")
	assert.Equal(t, "d", sl.firstInRange(rangeSpec{min: 5, max: 5}).ele, "a closed point range holds its point")

	empty := newSkiplist()
	assert.False(t, empty.overlaps(rangeSpec{min: 0, max: 1}))
	assert.Nil(t, empty.firstInRange(rangeSpec{min: 0, max: 1}))
	assert.Nil(t, empty.lastInRange(rangeSpec{min: 0, max: 1}))
}

func TestSkiplistLevelsAreGeometricAndBounded(t *testing.T) {
	sl := newSkiplist()
	var total, ones int
	for i := 0; i < 20000; i++ {
		l := sl.randomLevel()
		assert.GreaterOrEqual(t, l, 1)
		assert.LessOrEqual(t, l, skiplistMaxLevel)
		total += l
		if l == 1 {
			ones++
		}
	}
	// With p = 1/4 the mean level is 1/(1-p) = 4/3 and three quarters of
	// nodes stop at level one.
	assert.InDelta(t, 4.0/3.0, float64(total)/20000, 0.03)
	assert.InDelta(t, 0.75, float64(ones)/20000, 0.02)
}

// TestSkiplistAgainstReference drives the list with random inserts, removes and
// rescores and checks every answer against a plain sorted slice.
func TestSkiplistAgainstReference(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	sl := newSkiplist()
	ref := map[string]float64{}

	snapshot := func() []scored {
		var out []scored
		for ele, score := range ref {
			out = append(out, scored{score, ele})
		}
		return sortedEntries(out)
	}

	for step := 0; step < 5000; step++ {
		ele := string(rune('a'+rng.Intn(26))) + string(rune('a'+rng.Intn(26)))
		score := float64(rng.Intn(50))
		switch cur, present := ref[ele]; {
		case !present:
			sl.insert(score, ele)
			ref[ele] = score
		case rng.Intn(2) == 0:
			assert.True(t, sl.remove(cur, ele))
			delete(ref, ele)
		default:
			sl.updateScore(cur, ele, score)
			ref[ele] = score
		}

		if step%250 == 0 {
			want := snapshot()
			assert.Equal(t, want, walk(sl))
			for i, e := range want {
				assert.EqualValues(t, i+1, sl.rank(e.score, e.ele))
			}
		}
	}
	assert.EqualValues(t, len(ref), sl.length)
}
