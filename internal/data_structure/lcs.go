package data_structure

import (
	"math"
	"math/bits"

	"github.com/brandopakel/keel/internal/config"
)

// Longest common subsequence.
//
// The textbook algorithm fills an (n+1) by (m+1) table of lengths and walks it
// backwards to recover the sequence. Redis does exactly that, and pays for the
// table: two 11KB values need 512MB of transient allocation, which is precisely
// where Redis gives up, because that is its proto-max-bulk-len.
//
// What is returned has to be what Redis returns. A longest common subsequence
// is not unique - "ab" and "ba" share one of length one, and either character
// will do - and IDX goes further and reports where each matched character was
// placed, of which there are often several choices even for the same
// subsequence. Redis's answer is whatever its backward walk arrives at: from
// the bottom right corner, take the diagonal where the two characters match,
// otherwise step towards the larger neighbour, and on a tie step back through
// the second string. A client that diffs two values on two servers has no way
// to tell which answer is right, so this walks the same path.
//
// It walks it without the table. Two facts make that cheap. First, a row of
// the table rises by at most one from each column to the next, so a row is
// fully described by one bit per column saying whether it rose there: 1/32 of
// the uint32 Redis keeps. Second, the walk's decision at a cell is a function
// of those bits alone - see walk - so the bits are all it needs. And rather
// than keep every row even as bits, the forward pass keeps one row in every k,
// with k about the square root of the row count, and the walk recomputes the k
// rows of the stretch it is in from the checkpoint above it as it reaches
// them. That is the forward pass done twice in total, the same 2nm the
// divide-and-conquer this replaced cost, for (2*sqrt(n)+2) bit rows plus two
// rows of lengths for the forward pass.
//
// Rows run along the shorter string, so for two 10,000-byte strings that is
// about 330KB, against the 400MB the table would have been; for a 10MB value
// against a short one it is a few kilobytes. That is what moves the limit in
// config.LCSMaxCells from being about memory to being about time.
//
// Hirschberg's divide-and-conquer, which this replaced, kept two rows rather
// than a square root of them and returned Redis's subsequence, but chose its
// own placement among equally good ones: its IDX ranges differed from Redis's
// on 27% of 2008 sampled pairs. A placement is the output, so it is Redis's.

// LCSTooLarge reports whether a pair of strings would cost more cell
// comparisons than config.LCSMaxCells allows.
//
// The bound is on time rather than space, which is the whole reason it can be
// as generous as it is - see the comment on that setting. What matters here is
// that the cost is the product of the two lengths and not the larger of them:
// a 10MB value against a three-character one is cheap.
func LCSTooLarge(a, b string) bool {
	return config.LCSMaxCells > 0 && uint64(len(a))*uint64(len(b)) > config.LCSMaxCells
}

// LCSMatch is one run of characters common to both strings, as the ranges it
// occupies in each. Ranges are inclusive, matching what Redis reports.
type LCSMatch struct {
	AStart, AEnd int
	BStart, BEnd int
}

func (m LCSMatch) Len() int { return m.AEnd - m.AStart + 1 }

// LCSLen returns the length of the longest common subsequence.
//
// This is the two-row form: nothing is recovered, so nothing has to be
// remembered, and it does half the work of the full algorithm.
func LCSLen(a, b string) int {
	// Rows run along the shorter string, so memory is 8*min(n,m) whatever the
	// other one is. The length is symmetric, so the swap costs nothing.
	if len(a) < len(b) {
		a, b = b, a
	}
	if len(b) == 0 {
		return 0
	}

	prev := make([]int32, len(b)+1)
	cur := make([]int32, len(b)+1)
	for i := 1; i <= len(a); i++ {
		ai := a[i-1]
		cur[0] = 0
		for j := 1; j <= len(b); j++ {
			switch {
			case ai == b[j-1]:
				cur[j] = prev[j-1] + 1
			case prev[j] >= cur[j-1]:
				cur[j] = prev[j]
			default:
				cur[j] = cur[j-1]
			}
		}
		prev, cur = cur, prev
	}
	return int(prev[len(b)])
}

// LCSMatches returns the runs making up a longest common subsequence, in
// increasing position order, together with the subsequence itself: the same
// subsequence and the same runs Redis reports, for the reasons in the package
// comment.
func LCSMatches(a, b string) ([]LCSMatch, string) {
	if len(a) == 0 || len(b) == 0 {
		return nil, ""
	}
	t := newLCSTable(a, b)
	pairs := t.walk()
	seq := make([]byte, len(pairs))
	for k, p := range pairs {
		seq[k] = a[p.i]
	}
	return runsOf(pairs), string(seq)
}

// LCSWorkspaceBytes is what LCSMatches allocates for its rows, for a caller
// that reserves memory before calling it. The pairs, runs and subsequence it
// returns are on top, each at most one entry per character of the shorter
// string.
func LCSWorkspaceBytes(n, m int) int {
	long, short := max(n, m), min(n, m)
	if short == 0 {
		return 0
	}
	stride := lcsStride(long)
	words := (short + 63) / 64
	return 8*words*(long/stride+1+stride+1) + 4*2*(short+1)
}

type lcsPair struct{ i, j int }

// lcsStride is how many rows lie between checkpoints: the square root of the
// row count, which minimises checkpoints plus the stretch recomputed from one.
func lcsStride(rows int) int {
	k := int(math.Sqrt(float64(rows)))
	for k*k < rows {
		k++
	}
	return max(k, 1)
}

// lcsTable is the table of a against b, as checkpointed bit rows.
//
// Its rows are over y, the shorter string, one per character of x, the longer
// one - which is a against b or, when b is the longer, b against a. Row r, bit
// c-1 is set when LCS(x[:r], y[:c]) is one more than LCS(x[:r], y[:c-1]).
type lcsTable struct {
	x, y string
	// swapped is whether x is b. The walk is Redis's in a and b, so it has to
	// know which of x and y each one is.
	swapped bool
	stride  int
	words   int
	// checkpoints holds rows 0, stride, 2*stride ... of the table. block holds
	// rows blockBase to filled, recomputed from the checkpoint at blockBase as
	// the walk reaches them; filled is 0 until the walk starts.
	checkpoints, block []uint64
	blockBase, filled  int
	// prev and cur are the lengths of two adjacent rows, which is what the
	// recurrence needs to produce the next one.
	prev, cur []int32
	// total is the length of the whole subsequence.
	total int
}

func newLCSTable(a, b string) *lcsTable {
	t := &lcsTable{x: a, y: b}
	if len(a) < len(b) {
		t.x, t.y, t.swapped = b, a, true
	}
	n := len(t.x)
	t.stride = lcsStride(n)
	t.words = (len(t.y) + 63) / 64
	t.checkpoints = make([]uint64, t.words*(n/t.stride+1))
	t.block = make([]uint64, t.words*(t.stride+1))
	t.prev = make([]int32, len(t.y)+1)
	t.cur = make([]int32, len(t.y)+1)

	// Row 0 is all zeros, and so is its checkpoint. Every later row is
	// computed from the one before, and every stride-th is kept.
	for r := 1; r <= n; r++ {
		t.step(t.x[r-1])
		if r%t.stride == 0 {
			t.store(t.checkpoints[r/t.stride*t.words:], t.prev)
		}
	}
	t.total = int(t.prev[len(t.prev)-1])
	return t
}

// step advances prev from row r-1 to row r, whose character in x is c.
func (t *lcsTable) step(c byte) {
	prev, cur := t.prev, t.cur
	cur[0] = 0
	for j := 1; j < len(cur); j++ {
		v := cur[j-1]
		switch {
		case c == t.y[j-1]:
			v = prev[j-1] + 1
		case prev[j] >= v:
			v = prev[j]
		}
		cur[j] = v
	}
	t.prev, t.cur = cur, prev
}

// store writes the lengths of one row as its rise bits. Adjacent lengths
// differ by 0 or 1, so the difference is the bit.
func (t *lcsTable) store(dst []uint64, lengths []int32) {
	for w := 0; w < t.words; w++ {
		var word uint64
		first := w*64 + 1
		for c := first; c < min(first+64, len(lengths)); c++ {
			word |= uint64(lengths[c]-lengths[c-1]) << uint(c-first)
		}
		dst[w] = word
	}
}

// load reads a checkpointed row back into prev as lengths, to compute on from.
func (t *lcsTable) load(src []uint64) {
	t.prev[0] = 0
	for c := 1; c < len(t.prev); c++ {
		t.prev[c] = t.prev[c-1] + int32(src[(c-1)/64]>>uint((c-1)%64)&1)
	}
}

// fill recomputes the block for the stretch of rows ending at row end, from
// the checkpoint above it.
func (t *lcsTable) fill(end int) {
	t.blockBase, t.filled = (end-1)/t.stride*t.stride, end
	t.load(t.checkpoints[t.blockBase/t.stride*t.words:])
	copy(t.block, t.checkpoints[t.blockBase/t.stride*t.words:][:t.words])
	for r := t.blockBase + 1; r <= end; r++ {
		t.stepStore(t.x[r-1], t.block[(r-t.blockBase)*t.words:])
	}
}

// stepStore is step and store together: the rise bits are packed as the row
// is computed, rather than in a second pass over it, because the walk
// recomputes every row and this is half of its work.
func (t *lcsTable) stepStore(c byte, dst []uint64) {
	prev, cur := t.prev, t.cur
	cur[0] = 0
	var word uint64
	for j := 1; j < len(cur); j++ {
		v := cur[j-1]
		switch {
		case c == t.y[j-1]:
			v = prev[j-1] + 1
		case prev[j] >= v:
			v = prev[j]
		}
		word |= uint64(v-cur[j-1]) << uint((j-1)&63)
		cur[j] = v
		if j&63 == 0 {
			dst[(j-1)>>6] = word
			word = 0
		}
	}
	if (len(cur)-1)&63 != 0 {
		dst[(len(cur)-2)>>6] = word
	}
	t.prev, t.cur = cur, prev
}

// rose reports bit c-1 of row r, which must be in the block: whether the row
// rises at column c.
func (t *lcsTable) rose(r, c int) bool {
	row := t.block[(r-t.blockBase)*t.words:]
	return row[(c-1)/64]>>uint((c-1)%64)&1 == 1
}

// length is LCS(x[:r], y[:c]) for a row in the block: the rises up to c.
func (t *lcsTable) length(r, c int) int {
	row := t.block[(r-t.blockBase)*t.words:]
	n := 0
	for w := 0; w < c/64; w++ {
		n += bits.OnesCount64(row[w])
	}
	if rest := c % 64; rest != 0 {
		n += bits.OnesCount64(row[c/64] & (1<<uint(rest) - 1))
	}
	return n
}

// walk is Redis's backward walk, returning the matched pairs in increasing
// order of position.
//
// Where the characters at a cell (i, j) of a against b differ, Redis steps
// back through a if LCS(i-1, j) > LCS(i, j-1), and otherwise back through b.
// The cell's own length v is the larger of the two neighbours, and the
// neighbour along the row is v-1 where the row rises at the cell and v where
// it does not, so the rise bit nearly decides it alone:
//
//   - Rows along b (x is a): a row that does not rise has LCS(i, j-1) = v,
//     which LCS(i-1, j) cannot beat, so the walk steps back through b. A row
//     that rises has LCS(i, j-1) = v-1, so the neighbour above is v and wins.
//   - Rows along a (x is b): the neighbour along the row is LCS(i-1, j). If
//     the row rises it is v-1 and loses, and the walk steps back through b,
//     which is up the table. If not, it is v and wins - unless the neighbour
//     above is v as well, a tie, which Redis gives to b. Only then is the
//     length above counted.
func (t *lcsTable) walk() []lcsPair {
	x, y, v := len(t.x), len(t.y), t.total
	rev := make([]lcsPair, 0, t.total)
	for x > 0 && y > 0 {
		if x > t.filled || x <= t.blockBase {
			t.fill(x)
		}
		switch {
		case t.x[x-1] == t.y[y-1]:
			rev = append(rev, t.pair(x-1, y-1))
			x, y, v = x-1, y-1, v-1
		case !t.swapped && !t.rose(x, y):
			y--
		case !t.swapped:
			x--
		case !t.rose(x, y) && t.length(x-1, y) < v:
			y--
		default:
			x--
		}
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev
}

// pair is a matched cell of the table as positions in a and b.
func (t *lcsTable) pair(xi, yi int) lcsPair {
	if t.swapped {
		return lcsPair{yi, xi}
	}
	return lcsPair{xi, yi}
}

// runsOf collapses consecutive pairs into ranges. Adjacent matched characters
// are one match rather than several, which is what makes the IDX output useful:
// a shared word reads as a word. It is also how Redis reports them: its walk
// extends a range while matches stay adjacent in both strings.
func runsOf(pairs []lcsPair) []LCSMatch {
	var out []LCSMatch
	for k := 0; k < len(pairs); k++ {
		start := k
		for k+1 < len(pairs) && pairs[k+1].i == pairs[k].i+1 && pairs[k+1].j == pairs[k].j+1 {
			k++
		}
		out = append(out, LCSMatch{
			AStart: pairs[start].i, AEnd: pairs[k].i,
			BStart: pairs[start].j, BEnd: pairs[k].j,
		})
	}
	return out
}
