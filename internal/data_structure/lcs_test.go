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
// ranges alike, so it can stand in for a live server; TestLCSRangesFromRedis
// pins some of those answers directly.
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
	return runsOf(pairs), string(seq)
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

// TestLCSRangesFromRedis pins IDX answers captured from a real Redis 8.10.1:
// the subsequence and where each run of it was placed in both strings. The
// longer pairs span several 64-bit words of a row and several checkpoint
// stretches, with the longer string first and second, and over two letters
// almost every placement is a choice.
func TestLCSRangesFromRedis(t *testing.T) {
	for _, g := range []struct {
		a, b, seq string
		runs      []LCSMatch
	}{
		{"ohmytext", "mynewtext", "mytext", []LCSMatch{{2, 3, 0, 1}, {4, 7, 5, 8}}},
		{"ab", "ba", "b", []LCSMatch{{1, 1, 0, 0}}},
		{"abbab", "aaabaab", "abab", []LCSMatch{{0, 0, 0, 0}, {2, 2, 3, 3}, {3, 4, 5, 6}}},
		{"baccaaca", "aaccacbbba", "accaca", []LCSMatch{{1, 3, 1, 3}, {5, 6, 4, 5}, {7, 7, 9, 9}}},
		{"caaabacbcbabcc", "cbccbbabaabbac", "cbccbabc", []LCSMatch{{0, 0, 0, 0}, {4, 4, 1, 1}, {6, 6, 2, 2}, {8, 8, 3, 3}, {9, 11, 5, 7}, {13, 13, 13, 13}}},
		{"a", "bbba", "a", []LCSMatch{{0, 0, 3, 3}}},
		{"bbba", "a", "a", []LCSMatch{{3, 3, 0, 0}}},
		{"aabbbabbbbbababbaaaaaabaaabbbbbabbbabaaabbabababaaabbbbbabbaaaaaaaaaaa", "bababbbbabbaabbbbbbbbbbaabbababaaabababaabbabbbbabbabbbbbbaaabbabbbbbabababbbbababaaabaabbbabbbbbbabbbbbababbbbbbbbaaabbabababbbaaabbabbbbab", "aabbbabbbbbbabbaaaaaabaaabbbbbabbbabaaabbabababaaabbbbbabbaaaaaaaaaaa", []LCSMatch{{0, 0, 1, 1}, {1, 1, 3, 3}, {2, 7, 5, 10}, {8, 10, 13, 15}, {12, 12, 22, 22}, {13, 13, 24, 24}, {14, 14, 26, 26}, {15, 16, 28, 29}, {17, 19, 31, 33}, {20, 20, 35, 35}, {21, 24, 37, 40}, {25, 25, 43, 43}, {26, 28, 45, 47}, {29, 31, 49, 51}, {32, 32, 57, 57}, {33, 35, 61, 63}, {36, 37, 68, 69}, {38, 38, 71, 71}, {39, 39, 73, 73}, {40, 45, 76, 81}, {46, 47, 84, 85}, {48, 48, 87, 87}, {49, 49, 91, 91}, {50, 50, 98, 98}, {51, 54, 100, 103}, {55, 56, 105, 106}, {57, 61, 113, 117}, {62, 62, 120, 120}, {63, 63, 122, 122}, {64, 64, 124, 124}, {65, 67, 128, 130}, {68, 68, 133, 133}, {69, 69, 138, 138}}},
		{"abbccabbbaaabacbaaababacbbccbbbcbacabcbbcababaaaccbbcbabcbbaabbacbbcbabccbcacccaabcbcacababcaabacbaababbccbbabcacbccabcbbcbcaacccaaababcabccaaacacabbb", "cacbcbbacbcacbcababaabcccccbccccbabcbcaaacccaccbaabcacabbabbbccccccccaacabcabbcbbabbababcc", "cabbbabacbabababcccccbccccbabcbaaacccaccbaabcaabbabbbccccccccaacabcabcaaacc", []LCSMatch{{4, 5, 0, 1}, {6, 6, 3, 3}, {7, 8, 5, 6}, {11, 11, 7, 7}, {12, 12, 9, 9}, {13, 15, 11, 13}, {18, 22, 15, 19}, {25, 27, 21, 23}, {31, 31, 24, 24}, {34, 34, 25, 25}, {37, 37, 26, 26}, {39, 40, 27, 28}, {48, 49, 29, 30}, {52, 56, 31, 35}, {58, 58, 36, 36}, {60, 60, 38, 38}, {63, 63, 39, 39}, {69, 69, 40, 40}, {71, 72, 41, 42}, {74, 75, 43, 44}, {78, 78, 45, 45}, {82, 83, 46, 47}, {87, 87, 48, 48}, {89, 92, 49, 52}, {93, 94, 54, 55}, {97, 97, 56, 56}, {99, 100, 57, 58}, {102, 103, 59, 60}, {105, 105, 61, 61}, {110, 110, 62, 62}, {112, 112, 63, 63}, {114, 115, 64, 65}, {118, 118, 66, 66}, {121, 121, 67, 67}, {123, 125, 68, 70}, {128, 128, 71, 71}, {133, 137, 72, 76}, {139, 139, 78, 78}, {140, 140, 81, 81}, {141, 141, 84, 84}, {142, 142, 86, 86}, {143, 143, 88, 88}, {145, 145, 89, 89}}},
		{"abbbbbaabbbbbaabaabaaaaabbabbbababbbaabaaaabbbbaaaaabaaaaaaabbbaabaabbabbbaaaababbbbaababbbaabbbbbabbbaaaaabbababbbaabaaabaabbbaaaaaabbbabbbbbbabbaaaabbaaaabaabaaabbaaabbaabbaaabaaaabaabaabbbbabbbaabb", "aabbaababababbbaabaaababbbabbabbabbabbaababbbbbaaaaaaaabbaaabbaabbbaabbababaaabaabaabababababaababbabbabbbaabaabaaabaabbbbaabbbabbbaaabbaaabbabaaabbbabbabbaabbbbabababbbbbbbaabaaaaabbaaabbbbbaaabbaaaa", "abbaabbbbbbaabaaababbbabbbbabaaaabbbbaaaaaaaaaaabbaabaabbabbbaaaababbbbaababbabbbbbabaaaaabaabbbbaababbbaaaaaabbabbbbbbabbaabbbbaaabbbbbbaabaaaaabaabbbbbaabb", []LCSMatch{{0, 0, 0, 0}, {4, 8, 2, 6}, {9, 9, 8, 8}, {10, 10, 10, 10}, {11, 12, 12, 13}, {15, 18, 14, 17}, {21, 23, 18, 20}, {25, 31, 21, 27}, {33, 33, 28, 28}, {34, 35, 30, 31}, {37, 37, 32, 32}, {38, 39, 34, 35}, {40, 41, 38, 39}, {42, 46, 41, 45}, {48, 51, 47, 50}, {53, 56, 51, 54}, {57, 59, 57, 59}, {61, 64, 60, 63}, {65, 71, 66, 72}, {72, 72, 74, 74}, {73, 75, 78, 80}, {76, 80, 82, 86}, {81, 81, 88, 88}, {82, 82, 90, 90}, {83, 87, 92, 96}, {89, 90, 97, 98}, {92, 94, 99, 101}, {95, 98, 103, 106}, {101, 103, 108, 110}, {104, 106, 112, 114}, {108, 109, 115, 116}, {111, 114, 117, 120}, {117, 117, 121, 121}, {119, 121, 122, 124}, {123, 129, 127, 133}, {130, 132, 136, 138}, {134, 137, 139, 142}, {138, 140, 146, 148}, {141, 145, 150, 154}, {148, 151, 155, 158}, {156, 156, 159, 159}, {159, 160, 160, 161}, {161, 161, 163, 163}, {162, 164, 165, 167}, {168, 169, 168, 169}, {172, 173, 170, 171}, {175, 177, 173, 175}, {179, 181, 176, 178}, {183, 184, 179, 180}, {185, 187, 182, 184}, {190, 191, 186, 187}, {193, 195, 188, 190}, {196, 199, 192, 195}}},
		{"cacdcaabaaacdcdbbacdadcbbdcbcaacdcbabbacbdcdcbdcdbdbdaddbdddbacbccbcddabbcbbadabababbacbbbadcddbadacacaacbdbcadacdbabbdddbadcdcdcc", "dabcbbbaaacccdbbbcdadacadcacaddacaddacccdacdacdcadcdacdbadbdaddcbabcddaddbbdbabdbacadadcdabaaddbcadaaaabbccdddaccccddbbdbbbabdbcbccdadaabdbdacbdccbcdcabddbdbdddaaddcabccdabbadaadabdbddcdccdbaccacdbcdcabbdbbccadcaccdabccdbcddadcdbbbacdaacdcbddbaacdbbccadcdaabaaabaaaadbccaabcbdcaaaabccddbcbbbaccaadbdd", "cacdcaaaaacdcdacdadcdccaadcbabbabdcddcdbddadbdddaccccddabbcbadabababbacbbbadcddbaacacaacbdbcdacdbabbdddbadcdcdcc", []LCSMatch{{0, 0, 3, 3}, {1, 1, 9, 9}, {2, 3, 12, 13}, {4, 4, 17, 17}, {5, 5, 19, 19}, {6, 6, 21, 21}, {8, 8, 26, 26}, {9, 9, 28, 28}, {10, 11, 31, 32}, {12, 12, 35, 35}, {13, 14, 39, 40}, {17, 20, 41, 44}, {21, 22, 46, 47}, {25, 26, 49, 50}, {28, 28, 53, 53}, {29, 29, 56, 56}, {30, 30, 60, 60}, {32, 33, 62, 63}, {34, 34, 66, 66}, {35, 35, 70, 70}, {36, 36, 74, 74}, {37, 38, 76, 77}, {40, 41, 78, 79}, {42, 42, 82, 82}, {43, 43, 84, 84}, {46, 48, 86, 88}, {49, 49, 90, 90}, {50, 50, 93, 93}, {52, 52, 94, 94}, {53, 53, 97, 97}, {55, 55, 98, 98}, {56, 56, 104, 104}, {57, 59, 107, 109}, {61, 62, 110, 111}, {64, 65, 112, 113}, {67, 67, 114, 114}, {68, 68, 116, 116}, {69, 69, 119, 119}, {70, 71, 123, 124}, {72, 73, 126, 127}, {75, 75, 128, 128}, {76, 77, 132, 133}, {78, 78, 135, 135}, {79, 79, 138, 138}, {80, 80, 140, 140}, {81, 81, 146, 146}, {82, 82, 150, 150}, {83, 83, 154, 154}, {84, 84, 156, 156}, {85, 85, 161, 161}, {86, 86, 164, 164}, {87, 87, 166, 166}, {88, 89, 171, 172}, {90, 90, 178, 178}, {91, 93, 183, 185}, {94, 96, 188, 190}, {98, 98, 193, 193}, {99, 100, 199, 200}, {101, 102, 207, 208}, {103, 103, 211, 211}, {104, 104, 213, 213}, {105, 105, 216, 216}, {106, 108, 219, 221}, {110, 111, 223, 224}, {112, 113, 226, 227}, {114, 114, 230, 230}, {115, 115, 235, 235}, {116, 116, 239, 239}, {117, 117, 242, 242}, {118, 118, 246, 246}, {119, 119, 252, 252}, {120, 120, 254, 254}, {121, 121, 261, 261}, {122, 123, 265, 266}, {124, 124, 273, 273}, {125, 125, 275, 275}, {126, 126, 283, 283}, {127, 127, 285, 285}, {128, 129, 292, 293}}},
		{"abaaabaaa", "baaaabaabababbabbaabaababaaaaaaaaaabaabbabbabaaabaabbaabbbaabaababbababbbbbaabaaaaabaaaabaaababbbbaaaaaaabababbaaaaaabbbaabbbbbbabbaaabbbbbbbbbaaaabababbbbbbbaaabaaaaaaabbabaabbaabbbaabbbaaababbbbabbababaabbbaaaabbabababaaababbaaabbbabaaaaaabaabbaaab", "abaaabaaa", []LCSMatch{{0, 1, 233, 234}, {2, 2, 240, 240}, {3, 4, 242, 243}, {5, 8, 245, 248}}},
		{"ccbcbaccbaabaaccccaccabacababcbccbbcbbcccabcabaabcbcabbacabbbcaaaaaacbbcabbbbaabcbccabccccbcacbbcaacbbccabaaaacbaaacbccababbaacccbcaccbaacbcaacbcbbaabacbbcabaabbaaababacccccbcbbabccaccacabcabcabacbbaaaabbaabacabacabbbcaaaacaaaabbcbabbbcbabbcccaacccabaccabcbacb", "baacacc", "baacacc", []LCSMatch{{239, 239, 0, 0}, {248, 248, 1, 1}, {250, 250, 2, 2}, {252, 253, 3, 4}, {255, 255, 5, 5}, {258, 258, 6, 6}}},
	} {
		runs, seq := LCSMatches(g.a, g.b)
		assert.Equal(t, g.seq, seq, "LCS(%q, %q)", g.a, g.b)
		if len(g.runs) == 0 {
			g.runs = nil
		}
		assert.Equal(t, g.runs, runs, "LCS IDX(%q, %q)", g.a, g.b)
	}
}

// randomPairs generates the inputs the property tests run over: short strings
// from small alphabets, where a longest common subsequence is heavily ambiguous
// and any inconsistency in how one is chosen shows up quickly.
func randomPairs(t *testing.T, seed int64, each int, visit func(a, b string)) {
	t.Helper()
	randomPairsUpTo(t, seed, each, 14, visit)
}

// randomPairsUpTo is randomPairs with lengths up to max.
func randomPairsUpTo(t *testing.T, seed int64, each, max int, visit func(a, b string)) {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	for _, alpha := range []string{"ab", "abc", "abcdefgh", "ab cdefghijklmnop"} {
		for trial := 0; trial < each; trial++ {
			mk := func() string {
				s := make([]byte, rng.Intn(max))
				for i := range s {
					s[i] = alpha[rng.Intn(len(alpha))]
				}
				return string(s)
			}
			visit(mk(), mk())
		}
	}
}

// TestLCSAgreesWithRedis is the compatibility contract.
//
// The length, the subsequence and the IDX ranges must all be what Redis
// returns, not merely as long: a client that diffs two values and gets a
// different answer from two servers has no way to tell which is right. Short
// pairs are where the choices are densest; the longer ones cross word and
// checkpoint boundaries, which short ones never reach.
func TestLCSAgreesWithRedis(t *testing.T) {
	checked := 0
	check := func(a, b string) {
		gotRuns, got := LCSMatches(a, b)
		wantRuns, want := redisLCS(a, b)
		if got != want {
			t.Errorf("sequence differs: a=%q b=%q got=%q want=%q", a, b, got, want)
		}
		if !assert.ObjectsAreEqual(wantRuns, gotRuns) {
			t.Errorf("ranges differ: a=%q b=%q got=%v want=%v", a, b, gotRuns, wantRuns)
		}
		if n := LCSLen(a, b); n != len(want) {
			t.Errorf("length differs: a=%q b=%q got=%d want=%d", a, b, n, len(want))
		}
		checked++
	}
	randomPairs(t, 7, 3000, check)
	randomPairsUpTo(t, 17, 60, 400, check)
	t.Logf("checked %d pairs against the Redis model", checked)
}

// TestLCSRangesDescribeTheSequenceTheyCameFrom.
//
// Independently of agreeing with Redis, the ranges must be a correct
// description of the subsequence that was returned: ordered, non-overlapping,
// contiguous within a run, and naming characters that really are equal in both
// strings.
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

// TestLCSMemoryIsFarBelowTheTable is the reason for the whole construction.
//
// The table Redis builds for these two strings would be 400MB. What this
// allocates has to stay in the neighbourhood of the strings themselves, and in
// particular must not grow with their product: checkpointed bit rows grow with
// the shorter string times the square root of the longer.
func TestLCSMemoryIsFarBelowTheTable(t *testing.T) {
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
	rows := uint64(LCSWorkspaceBytes(n, n))
	assert.Less(t, used, rows+uint64(64*len(seq))+1<<20, "the reserved workspace, the pairs and slack cover the call")

	// A long value against a short one stays small: the rows run along the
	// short one.
	long := strings.Repeat("ab", 500000)
	runtime.ReadMemStats(&before)
	LCSMatches(long, "ba")
	runtime.ReadMemStats(&after)
	assert.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(64<<10))
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
