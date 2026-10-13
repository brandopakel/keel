package data_structure

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func lexRange(t *testing.T, min, max string) LexRange {
	t.Helper()
	lo, ok1 := ParseLexBound(min)
	hi, ok2 := ParseLexBound(max)
	require.True(t, ok1 && ok2, "%s %s", min, max)
	return LexRange{Min: lo, Max: hi}
}

func lexMembers(zs *ZSet, r LexRange, offset, count int, reverse bool) []string {
	var out []string
	zs.VisitRangeByLex(r, offset, count, reverse, func(member string, _ float64) bool {
		out = append(out, member)
		return true
	})
	return out
}

// TestLexRangesAsRedisReadsThem: inclusive and exclusive ends, the two
// infinities, an empty or inverted range, offsets and counts in both
// directions, as ZRANGEBYLEX and ZLEXCOUNT answer them.
func TestLexRangesAsRedisReadsThem(t *testing.T) {
	t.Parallel()
	zs := CreateZSet()
	for _, m := range []string{"a", "b", "c", "d", "e", "f"} {
		zs.Add(0, m, 0)
	}
	for _, tc := range []struct {
		min, max      string
		offset, count int
		reverse       bool
		want          []string
	}{
		{"-", "+", 0, -1, false, []string{"a", "b", "c", "d", "e", "f"}},
		{"[b", "(e", 0, -1, false, []string{"b", "c", "d"}},
		{"(b", "[e", 1, 2, false, []string{"d", "e"}},
		{"[e", "(b", 0, -1, true, []string{"e", "d", "c"}},
		{"+", "-", 1, 2, true, []string{"e", "d"}},
		{"+", "-", 0, -1, false, nil},
		{"[c", "[c", 0, -1, false, []string{"c"}},
		{"(c", "[c", 0, -1, false, nil},
		{"-", "+", -1, 2, false, nil},
		{"-", "+", 0, 0, false, nil},
		{"(z", "+", 0, -1, false, nil},
	} {
		r := lexRange(t, tc.min, tc.max)
		if tc.reverse {
			r = lexRange(t, tc.max, tc.min)
		}
		require.Equal(t, tc.want, lexMembers(zs, r, tc.offset, tc.count, tc.reverse), "%+v", tc)
	}
	require.EqualValues(t, 6, zs.CountByLex(lexRange(t, "-", "+")))
	require.EqualValues(t, 3, zs.CountByLex(lexRange(t, "[b", "(e")))
	require.EqualValues(t, 0, zs.CountByLex(lexRange(t, "(z", "+")))
	for _, bad := range []string{"b", "", "x("} {
		_, ok := ParseLexBound(bad)
		require.False(t, ok, bad)
	}
	member, score, ok := zs.MemberAtRank(2)
	require.True(t, ok)
	require.Equal(t, "c", member)
	require.Zero(t, score)
	_, _, ok = zs.MemberAtRank(6)
	require.False(t, ok)
}

// TestAddScoreAppliesGTLTAndIncr: AddScore's outcomes, as Redis's zsetAdd
// reports them.
func TestAddScoreAppliesGTLTAndIncr(t *testing.T) {
	t.Parallel()
	zs := CreateZSet()
	res, s, nan := zs.AddScore(5, "a", 0)
	require.Equal(t, ZAddAdded, res)
	require.Equal(t, 5.0, s)
	require.False(t, nan)
	res, s, _ = zs.AddScore(3, "a", ZAddGT)
	require.Equal(t, ZAddNop, res)
	require.Equal(t, 5.0, s)
	res, s, _ = zs.AddScore(7, "a", ZAddGT)
	require.Equal(t, ZAddUpdated, res)
	require.Equal(t, 7.0, s)
	res, _, _ = zs.AddScore(9, "a", ZAddLT)
	require.Equal(t, ZAddNop, res)
	res, s, _ = zs.AddScore(0, "a", ZAddIncr)
	require.Equal(t, ZAddUnchanged, res)
	require.Equal(t, 7.0, s)
	res, s, _ = zs.AddScore(-2, "a", ZAddIncr|ZAddLT)
	require.Equal(t, ZAddUpdated, res)
	require.Equal(t, 5.0, s)
	res, _, _ = zs.AddScore(1, "b", ZAddXX)
	require.Equal(t, ZAddNop, res)
	zs.AddScore(math.Inf(1), "c", 0)
	_, _, nan = zs.AddScore(math.Inf(-1), "c", ZAddIncr)
	require.True(t, nan)
	score, _ := zs.Score("c")
	require.Equal(t, math.Inf(1), score)
}
