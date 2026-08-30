package data_structure

import (
	"math/rand"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// redisLCS is the algorithm Redis uses: the full (n+1)x(m+1) table, walked
// backwards from the bottom right corner, taking a diagonal whenever the two
// characters match and otherwise stepping in whichever direction the table says
// costs nothing - preferring b on a tie.
//
// It is here as the thing to be tested against, not as an implementation: it
// allocates the table that lcs.go exists to avoid. It was checked against a
// real Redis 8.10.1 over 2008 pairs and agreed on every one, sequence and index
// ranges alike, so it can stand in for a live server.
func redisLCS(a, b string) ([]LCSMatch, string) {
	n, m := len(a), len(b)
	tbl := make([][]int, n+1)
	for i := range tbl {
		tbl[i] = make([]int, m+1)
	}
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			switch {
			case a[i-1] == b[j-1]:
				tbl[i][j] = tbl[i-1][j-1] + 1
			case tbl[i-1][j] > tbl[i][j-1]:
				tbl[i][j] = tbl[i-1][j]
			default:
				tbl[i][j] = tbl[i][j-1]
			}
		}
	}

	var rev []lcsPair
	i, j := n, m
	for i > 0 && j > 0 {
		switch {
		case a[i-1] == b[j-1]:
			rev = append(rev, lcsPair{i - 1, j - 1})
			i--
			j--
		case tbl[i-1][j] > tbl[i][j-1]:
			i--
		default:
			j--
		}
	}
	pairs := make([]lcsPair, len(rev))
	for k := range rev {
		pairs[k] = rev[len(rev)-1-k]
	}
	seq := make([]byte, len(pairs))
	for k, p := range pairs {
		seq[k] = a[p.i]
	}
	return runsOf(pairs, false), string(seq)
}

// TestLCSGoldenFromRedis pins the output against answers captured from a real
// Redis 8.10.1, so a regression cannot be hidden by a bug shared with the model
// above.
func TestLCSGoldenFromRedis(t *testing.T) {
	golden := []struct {
		a, b, seq string
		length    int
	}{
		{"ohmytext", "mynewtext", "mytext", 6},
		{"", "", "", 0},
		{"abc", "", "", 0},
		{"", "abc", "", 0},
		{"abc", "abc", "abc", 3},
		{"a", "a", "a", 1},
		{"aaa", "aa", "aa", 2},
		{"abcdef", "fedcba", "f", 1},
		{"acacbbcaababba", "abaaab", "abaaab", 6},
		{"peajn", "eocnogc", "en", 2},
		{"", "fd", "", 0},
		{"abbab", "aaabaab", "abab", 4},
		{"cbaaabc", "b", "b", 1},
		{"apple gamma delta echo echo apple", "cherry fox delta banana banana gamma", "e  delta   a", 12},
		{"njmbimoda", "mfhgnm", "mm", 2},
		{"febbafcghdebac", "aga", "aga", 3},
		{"hhaln", "eopc adp", "a", 1},
		{"fa", "hbhedfdhgfcde", "f", 1},
		{"a", "bbba", "a", 1},
		{"jbkdn", "fdjiakfidnknk", "jkdn", 4},
		{"b", "baabbaaabbbbba", "b", 1},
		{"fox hotel hotel gamma delta apple", "cherry apple gamma fox fox delta", "he l gamma delta", 16},
		{"apple fox apple hotel delta banana", "gamma delta hotel banana banana cherry", "aae hotel a banana", 18},
		{"g", "eddcggedabab", "g", 1},
		{"baccaaca", "aaccacbbba", "accaca", 6},
		{"gfaecb", "hdcfaehch", "faec", 4},
		{"caaabacbcbabcc", "cbccbbabaabbac", "cbccbabc", 8},
		{"abaabb", "abaabbbaabbbba", "abaabb", 6},
		{"ddbj", "hcaphi olnejjk", "j", 1},
		{"hbbcaefggfhhb", "dgacaceecc", "cae", 3},
		{"gaaacea", "hedbeaa", "ea", 2},
		{"echo delta fox banana fox banana", "echo hotel banana cherry banana banana", "echo ela  banana banana", 23},
		{"d", "adc", "d", 1},
		{"dahbaabdf", "eaaadeb", "aaad", 4},
		{"", "dfgbaefdc", "", 0},
		{"jie lcojd", "egd cgm ", "e c", 3},
		{"apple delta echo banana echo cherry", "cherry gamma cherry banana gamma apple", "e a ch banana  e", 16},
		{"", "bbbbaaaaaba", "", 0},
		{"c", "abcbababaa", "c", 1},
		{"eganjcipldga", "alaodnee", "ala", 3},
	}
	for _, g := range golden {
		_, seq := LCSMatches(g.a, g.b)
		assert.Equal(t, g.seq, seq, "LCS(%q, %q)", g.a, g.b)
		assert.Equal(t, g.length, LCSLen(g.a, g.b), "LCS LEN(%q, %q)", g.a, g.b)
	}
}

// randomPairs generates the inputs the property tests run over: short strings
// from small alphabets, where a longest common subsequence is heavily ambiguous
// and any inconsistency in how one is chosen shows up quickly.
func randomPairs(t *testing.T, seed int64, each int, visit func(a, b string)) {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	for _, alpha := range []string{"ab", "abc", "abcdefgh", "ab cdefghijklmnop"} {
		for trial := 0; trial < each; trial++ {
			mk := func() string {
				s := make([]byte, rng.Intn(14))
				for i := range s {
					s[i] = alpha[rng.Intn(len(alpha))]
				}
				return string(s)
			}
			visit(mk(), mk())
		}
	}
}

// TestLCSAgreesWithRedisOnLengthAndSequence is the compatibility contract.
//
// Both the length and the subsequence itself must match what Redis returns, not
// merely be as long: a client that diffs two values and gets a different answer
// from two servers has no way to tell which is right.
func TestLCSAgreesWithRedisOnLengthAndSequence(t *testing.T) {
	checked := 0
	randomPairs(t, 7, 3000, func(a, b string) {
		_, got := LCSMatches(a, b)
		_, want := redisLCS(a, b)
		if got != want {
			t.Errorf("sequence differs: a=%q b=%q got=%q want=%q", a, b, got, want)
		}
		if n := LCSLen(a, b); n != len(want) {
			t.Errorf("length differs: a=%q b=%q got=%d want=%d", a, b, n, len(want))
		}
		checked++
	})
	t.Logf("checked %d pairs against the Redis model", checked)
}

// TestLCSRangesDescribeTheSequenceTheyCameFrom.
//
// The ranges are where compatibility stops. Redis's positions come from walking
// the table backwards, and this does not build the table, so when several
// alignments give the same subsequence the two can place it differently. What
// must hold is that the ranges are a correct description of the subsequence
// that was returned: ordered, non-overlapping, contiguous within a run, and
// naming characters that really are equal in both strings.
func TestLCSRangesDescribeTheSequenceTheyCameFrom(t *testing.T) {
	randomPairs(t, 11, 2000, func(a, b string) {
		runs, seq := LCSMatches(a, b)

		var rebuilt strings.Builder
		prevA, prevB := -1, -1
		for _, r := range runs {
			assert.Greater(t, r.AStart, prevA, "ranges in a must advance: a=%q b=%q", a, b)
			assert.Greater(t, r.BStart, prevB, "ranges in b must advance: a=%q b=%q", a, b)
			assert.Equal(t, r.AEnd-r.AStart, r.BEnd-r.BStart, "a run is the same length in both")
			assert.Equal(t, a[r.AStart:r.AEnd+1], b[r.BStart:r.BEnd+1],
				"a run must name characters that match: a=%q b=%q", a, b)
			rebuilt.WriteString(a[r.AStart : r.AEnd+1])
			prevA, prevB = r.AEnd, r.BEnd
		}
		assert.Equal(t, seq, rebuilt.String(), "the ranges must reconstruct the sequence: a=%q b=%q", a, b)
	})
}

// TestLCSRunsAreMaximal. Adjacent matched characters have to be reported as one
// range rather than several, or IDX output would be a character-by-character
// list and a shared word would not read as a word.
func TestLCSRunsAreMaximal(t *testing.T) {
	runs, seq := LCSMatches("ohmytext", "mynewtext")
	assert.Equal(t, "mytext", seq)
	assert.Equal(t, []LCSMatch{
		{AStart: 2, AEnd: 3, BStart: 0, BEnd: 1},
		{AStart: 4, AEnd: 7, BStart: 5, BEnd: 8},
	}, runs, "the example from the Redis documentation")
}

func TestLCSEdgeCases(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want string
	}{
		{"", "", ""},
		{"abc", "", ""},
		{"", "abc", ""},
		{"abc", "xyz", ""},
		{"abc", "abc", "abc"},
		{"a", "aaaa", "a"},
		{"aaaa", "a", "a"},
	} {
		runs, seq := LCSMatches(tc.a, tc.b)
		assert.Equal(t, tc.want, seq, "LCS(%q, %q)", tc.a, tc.b)
		assert.Equal(t, len(tc.want), LCSLen(tc.a, tc.b), "LEN(%q, %q)", tc.a, tc.b)
		if tc.want == "" {
			assert.Empty(t, runs)
		}
	}
}

// TestLCSIsSymmetricInLength. The length cannot depend on argument order, and
// the swap that keeps the rows over the shorter string must not change it.
func TestLCSIsSymmetricInLength(t *testing.T) {
	randomPairs(t, 13, 1500, func(a, b string) {
		assert.Equal(t, LCSLen(a, b), LCSLen(b, a), "a=%q b=%q", a, b)
		_, seq := LCSMatches(a, b)
		assert.Equal(t, len(seq), LCSLen(a, b), "a=%q b=%q", a, b)
	})
}

// TestLCSMemoryIsLinearNotQuadratic is the reason for the whole construction.
//
// The table Redis builds for these two strings would be 400MB. What this
// allocates has to stay in the neighbourhood of the strings themselves, and in
// particular must not grow with their product.
func TestLCSMemoryIsLinearNotQuadratic(t *testing.T) {
	const n = 10000
	rng := rand.New(rand.NewSource(5))
	mk := func() string {
		s := make([]byte, n)
		for i := range s {
			s[i] = byte('a' + rng.Intn(4))
		}
		return string(s)
	}
	a, b := mk(), mk()

	table := uint64(n+1) * uint64(n+1) * 4

	// Total bytes allocated over the call rather than the heap before and
	// after: the working buffers are freed on return, so a snapshot either side
	// would show nothing at all. A cumulative total is an upper bound on what
	// was ever live at once, which is the safe direction for this assertion.
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	runs, seq := LCSMatches(a, b)
	runtime.ReadMemStats(&after)
	used := after.TotalAlloc - before.TotalAlloc

	t.Logf("two %d-byte strings: LCS is %d long in %d runs, allocated %d bytes against a %d byte table",
		n, len(seq), len(runs), used, table)
	assert.Less(t, used, table/100, "allocation must not be within two orders of magnitude of the table")
}

func TestLCSTooLargeGuardsTheEventLoop(t *testing.T) {
	small := strings.Repeat("x", 1000)
	assert.False(t, LCSTooLarge(small, small))

	big := strings.Repeat("x", 20000)
	assert.True(t, LCSTooLarge(big, big), "two 20KB strings are 400M cells, past the budget")

	// A long string against a short one is cheap, and must not be refused: the
	// cost is the product, not the larger of the two. Ten megabytes against
	// three characters is 30 million cells, well inside the budget.
	assert.False(t, LCSTooLarge(strings.Repeat("x", 10*1000*1000), "abc"))

	// The product is what counts, so the same ten megabytes against a kilobyte
	// is refused even though neither string grew.
	assert.True(t, LCSTooLarge(strings.Repeat("x", 10*1000*1000), strings.Repeat("y", 1000)))
}

func BenchmarkLCSLen(bench *testing.B) {
	a, b := lcsBenchInput(2000)
	bench.ResetTimer()
	for i := 0; i < bench.N; i++ {
		LCSLen(a, b)
	}
	bench.ReportMetric(float64(bench.N)*float64(len(a))*float64(len(b))/bench.Elapsed().Seconds(), "cells/s")
}

func BenchmarkLCSMatches(bench *testing.B) {
	a, b := lcsBenchInput(2000)
	bench.ResetTimer()
	for i := 0; i < bench.N; i++ {
		LCSMatches(a, b)
	}
	bench.ReportMetric(float64(bench.N)*float64(len(a))*float64(len(b))/bench.Elapsed().Seconds(), "cells/s")
}

func lcsBenchInput(n int) (string, string) {
	rng := rand.New(rand.NewSource(1))
	mk := func() string {
		s := make([]byte, n)
		for i := range s {
			s[i] = byte('a' + rng.Intn(26))
		}
		return string(s)
	}
	return mk(), mk()
}
