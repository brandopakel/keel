package data_structure

import (
	"math"

	"github.com/spaolacci/murmur3"
)

// Implementation of Morris's approximate counter.
// https://www.inf.ed.ac.uk/teaching/courses/exc/reading/morris.pdf
//
// The problem it solves: an exact counter needs enough bits to hold the largest
// value it will ever reach, and that width is paid on every counter whether or
// not it ever gets there. Counting to four billion exactly costs 32 bits per
// counter. Morris's observation in 1978 was that a counter which is allowed to
// be approximately right needs only log log n bits, because it can store the
// exponent rather than the number.
//
// A counter holds c, and stands for roughly (1+a)^c. Incrementing it means
// raising c, but only with probability (1+a)^-c - so the first few increments
// almost always take, and by the time c is large it takes millions of
// increments to move. The estimate read back is
//
//	((1+a)^c - 1)/a
//
// which is unbiased: the falling probability is exactly what makes the expected
// estimate equal the true count, for any number of increments. The parameter a
// buys accuracy with range. The relative standard error is sqrt(a/2), and eight
// bits reach ((1+a)^255 - 1)/a:
//
//	   a     relative sd     largest count
//	0.02          10.0%             7,750
//	0.05          15.8%         5,060,000
//	0.08          20.0%     4,290,000,000
//	0.125         25.0%   8.85 x 10^13
//	1.0           70.7%   5.79 x 10^76
//
// morrisA is 0.08. Eight bits then reach 4,168,383,430, within 3% of the
// 4,294,967,295 that a Count-Min sketch's 32-bit cell holds, at a relative
// standard error of 20%. That is the trade stated exactly: the same counting
// range as the sketch next door, at a quarter of the memory.
//
// Note this is the same trick as the LFU eviction counter in lfu.go, which is a
// Morris counter with a = LFULogFactor and no estimate ever read back - only
// ranked. Here the estimate is the point, so the parameter is chosen to make it
// unbiased rather than merely monotone.
const morrisA = 0.08

// morrisMaxExponent is the largest value a cell holds. Past it the counter
// saturates rather than wrapping, the way the LFU counter does: a wrap would
// turn the most-counted item into the least-counted one.
const morrisMaxExponent = math.MaxUint8

var (
	// morrisValue[c] is the count an exponent of c stands for, and
	// morrisProb[c] the probability that the next increment raises it.
	//
	// Both are functions of morrisA alone, so they are computed once for the
	// process rather than per sketch. Holding them per sketch would be 4KB of
	// tables against a table of counters that may only be a few hundred bytes.
	morrisValue [morrisMaxExponent + 1]uint64
	morrisProb  [morrisMaxExponent + 1]float64
)

func init() {
	for c := 0; c <= morrisMaxExponent; c++ {
		morrisProb[c] = math.Pow(1+morrisA, -float64(c))
		morrisValue[c] = uint64(math.Round((math.Pow(1+morrisA, float64(c)) - 1) / morrisA))
	}
	MorrisMaxCount = morrisValue[morrisMaxExponent]
}

// MorrisRelativeError is the relative standard error of a single counter,
// sqrt(a/2). Reported by MORRIS.INFO so the estimate can be read with the
// uncertainty that belongs to it.
var MorrisRelativeError = math.Sqrt(morrisA / 2)

// MorrisMaxCount is the largest count a saturated counter stands for.
//
// Assigned in init rather than here. Package-level initialisers run before init
// functions, and Go does not see a dependency on one, so reading morrisValue at
// this point would silently capture a zero.
var MorrisMaxCount uint64

// Morris is a table of approximate counters addressed by hashing, so that
// counting a stream of named items needs no memory per name.
//
// The addressing is the Count-Min sketch's: d rows of w counters, an item
// hashed once per row. What differs is the cell and how the rows are combined.
//
// A Count-Min sketch takes the minimum across rows because its cells are exact,
// so a collision can only ever inflate a cell and never deflate one, and the
// smallest row is the least contaminated. That reasoning does not survive a
// noisy cell. Morris error is symmetric - a cell is as likely to read low as
// high - so a minimum over d rows picks out whichever row happened to read
// lowest, which is not the cleanest row but the unluckiest one.
//
// Measured in morris_test.go with a table sized so collisions are negligible:
// the median lands within 2% of the true count, the minimum 21% to 26% below
// it. And the minimum gets worse as rows are added - 21% low at depth 5, 26%
// at depth 9 - because every extra row is another chance to read low. Depth is
// meant to buy accuracy, not spend it. The median is unbiased under symmetric
// noise and still unmoved by a minority of collided rows, so it keeps what
// depth is for.
type Morris struct {
	width      uint32
	depth      uint32
	totalCount uint64
	counters   []uint8

	// rngState drives the coin flips. A per-table xorshift rather than
	// math/rand keeps a table's behaviour reproducible and touches no shared
	// state, matching the cuckoo filter next door.
	rngState uint64
}

func CreateMorris(w uint32, d uint32) *Morris {
	if w < 1 {
		w = 1
	}
	if d < 1 {
		d = 1
	}
	return &Morris{
		width:    w,
		depth:    d,
		counters: make([]uint8, uint64(w)*uint64(d)),
		rngState: 0x9E3779B97F4A7C15,
	}
}

// CalcMorrisDim sizes a table the same way CalcCMSDim does, so that a Morris
// table and a Count-Min sketch asked for the same accuracy are the same shape
// and can be compared directly.
func CalcMorrisDim(errRate float64, errProb float64) (uint32, uint32) {
	return CalcCMSDim(errRate, errProb)
}

func (m *Morris) cell(item string, row uint32) uint64 {
	hasher := murmur3.New32WithSeed(row)
	hasher.Write([]byte(item))
	return uint64(hasher.Sum32()%m.width) + uint64(row)*uint64(m.width)
}

func (m *Morris) nextRand() uint64 {
	m.rngState ^= m.rngState << 13
	m.rngState ^= m.rngState >> 7
	m.rngState ^= m.rngState << 17
	return m.rngState
}

// nextFloat draws uniformly from (0, 1]. Zero is excluded because the caller
// takes its logarithm.
func (m *Morris) nextFloat() float64 {
	return (float64(m.nextRand()>>11) + 1) / float64(uint64(1)<<53)
}

// bump applies n increments to one cell.
//
// Flipping a coin n times would make MORRIS.INCRBY cost O(n), which for the
// large increments the command accepts is unusable - and pointless, since
// almost every flip fails once the counter is up. Instead the wait to the next
// success is drawn directly: with a fixed success probability p, the number of
// failures before a success is geometric, so ln(u)/ln(1-p) for a uniform u is
// distributed exactly as that wait. The loop then runs once per success rather
// than once per increment, which is at most 255 iterations however large n is.
func (m *Morris) bump(idx uint64, n uint64) {
	c := m.counters[idx]
	for n > 0 && c < morrisMaxExponent {
		p := morrisProb[c]
		var skip float64
		if p < 1 {
			// ln(1-p) via Log1p: p is tiny once the counter is up, and
			// 1-p would lose most of its significant digits there.
			skip = math.Log(m.nextFloat()) / math.Log1p(-p)
		}
		// Compared as a float because skip can exceed what a uint64 holds when
		// p is very small, and converting that is undefined.
		if skip >= float64(n) {
			break // no success within the increments we were given
		}
		n -= uint64(skip) + 1
		c++
	}
	m.counters[idx] = c
}

// IncrBy adds value to item's count and returns the new estimate.
func (m *Morris) IncrBy(item string, value uint64) uint64 {
	for row := uint32(0); row < m.depth; row++ {
		// Each row flips its own coins. Sharing them would make the rows move
		// together, and a median over identical rows is just one row.
		m.bump(m.cell(item, row), value)
	}
	m.totalCount += value
	return m.Count(item)
}

// Count returns the estimated number of times item has been counted.
func (m *Morris) Count(item string) uint64 {
	var stack [16]uint8
	var exps []uint8
	if int(m.depth) <= len(stack) {
		exps = stack[:m.depth]
	} else {
		exps = make([]uint8, m.depth)
	}
	for row := uint32(0); row < m.depth; row++ {
		exps[row] = m.counters[m.cell(item, row)]
	}
	return morrisValue[medianExponent(exps)]
}

// medianExponent sorts in place and returns the middle element. Insertion sort
// because depth is a handful of rows, where it beats anything cleverer, and
// because sort.Slice would allocate an interface per call on the read path.
func medianExponent(exps []uint8) uint8 {
	for i := 1; i < len(exps); i++ {
		v := exps[i]
		j := i - 1
		for j >= 0 && exps[j] > v {
			exps[j+1] = exps[j]
			j--
		}
		exps[j+1] = v
	}
	return exps[len(exps)/2]
}

func (m *Morris) Width() uint32      { return m.width }
func (m *Morris) Depth() uint32      { return m.depth }
func (m *Morris) TotalCount() uint64 { return m.totalCount }

// MemUsage estimates the bytes held. The table is fixed at creation, so this
// never changes for a given counter.
func (m *Morris) MemUsage() uint64 {
	return morrisBaseBytes + uint64(len(m.counters))
}
