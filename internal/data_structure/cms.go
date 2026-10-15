package data_structure

import (
	"errors"
	"math"

	"github.com/spaolacci/murmur3"
)

// CMS is a count-min sketch (Cormode and Muthukrishnan, 2005): a fixed table
// of counters that estimates how often each item has been seen, in memory that
// does not grow with the number of distinct items.
//
// The table has depth rows of width counters. Each row has its own hash
// function; incrementing an item adds to one counter in every row, and its
// estimate is the smallest of those counters. Collisions can only push a
// counter up, so the estimate never falls below the truth, and with width
// counters per row the amount it overshoots by is bounded: with probability
// 1-δ it is within ε of the total count, for
//
//	width = ceil(2/ε)    depth = ceil(log2(1/δ))
type CMS struct {
	width      uint32
	depth      uint32
	totalCount uint64
	// counter holds the rows one after another: row r, column c is at
	// r*width + c.
	counter []uint32
}

// CreateCMS makes a sketch of the given dimensions, both of which must be
// positive.
func CreateCMS(width, depth uint32) *CMS {
	return &CMS{
		width:   width,
		depth:   depth,
		counter: make([]uint32, uint64(width)*uint64(depth)),
	}
}

// CalcCMSDim is the width and depth that keep every estimate within errRate
// of the total count with probability 1 - errProb, as RedisBloom's
// CMS_DimFromProb works them out: the width in double precision, and the
// depth from log10f, in single precision, as C computes it, so that a
// sketch sized here has RedisBloom's dimensions for every rate.
func CalcCMSDim(errRate float64, errProb float64) (width, depth uint32) {
	w := math.Ceil(2.0 / errRate)
	d := math.Ceil(float64(log10f(errProb) / log10f(0.5)))
	if math.IsNaN(w) || math.IsNaN(d) || w < 1 || d < 1 || w > math.MaxUint32 || d > math.MaxUint32 {
		return 0, 0
	}
	width = uint32(w)
	depth = uint32(d)
	return width, depth
}

// log10f is C's log10f: x rounded to single precision, its logarithm rounded
// back to it.
func log10f(x float64) float32 { return float32(math.Log10(float64(float32(x)))) }

// cell is the counter an item lands in on one row. Each row hashes with its
// own seed; the seeds are the row numbers, which is what a sketch written by
// an earlier build hashed with, so it reads back meaning the same thing.
func (c *CMS) cell(item string, row uint32) uint64 {
	// The streaming form rather than Sum32WithSeed: the one-shot function in
	// this version of the library does pointer arithmetic that the race
	// detector's checkptr rejects on some input lengths.
	h := murmur3.New32WithSeed(row)
	h.Write([]byte(item))
	return uint64(row)*uint64(c.width) + uint64(h.Sum32()%c.width)
}

// IncrBy adds value to an item's counters, as RedisBloom's CMS_IncrBy does,
// and returns its new estimate, the smallest of them. A counter that cannot
// take the increment - its sum past 2^32 - 1, the 32 bits it is kept in -
// stops at that maximum rather than wrapping, and an estimate of the maximum
// is RedisBloom's mark of an overflow.
func (c *CMS) IncrBy(item string, value uint64) (estimate uint32) {
	estimate = math.MaxUint32
	for row := uint32(0); row < c.depth; row++ {
		at := c.cell(item, row)
		// RedisBloom adds in 64 bits and keeps the low 32, then saturates
		// what came out smaller than what was added.
		if sum := uint32(uint64(c.counter[at]) + value); uint64(sum) < value {
			c.counter[at] = math.MaxUint32
		} else {
			c.counter[at] = sum
		}
		if c.counter[at] < estimate {
			estimate = c.counter[at]
		}
	}
	c.totalCount += value
	return estimate
}

// ErrCMSMergeOverflow is a merge whose counters or total would not fit, in
// RedisBloom's words.
var ErrCMSMergeOverflow = errors.New("CMS: MERGE overflow")

// MergeFrom sets c's counters to the sum of the sources' counters, each
// multiplied by its weight, and its total count to the sum of theirs, as
// RedisBloom's CMS_Merge does; the sources have c's dimensions. A counter
// that would come out negative or past 32 bits, or a total that would come
// out negative or past 63, is ErrCMSMergeOverflow, and leaves c as it was.
func (c *CMS) MergeFrom(sources []*CMS, weights []int64) error {
	for cell := range c.counter {
		sum := int64(0)
		for k, src := range sources {
			product, ok := mulInt64(int64(src.counter[cell]), weights[k])
			if !ok {
				return ErrCMSMergeOverflow
			}
			if sum, ok = addInt64(sum, product); !ok {
				return ErrCMSMergeOverflow
			}
		}
		if sum < 0 || sum > math.MaxUint32 {
			return ErrCMSMergeOverflow
		}
	}
	total := int64(0)
	for k, src := range sources {
		// RedisBloom multiplies its size_t total into an int64.
		if src.totalCount > math.MaxInt64 {
			return ErrCMSMergeOverflow
		}
		product, ok := mulInt64(int64(src.totalCount), weights[k])
		if !ok {
			return ErrCMSMergeOverflow
		}
		if total, ok = addInt64(total, product); !ok {
			return ErrCMSMergeOverflow
		}
	}
	if total < 0 {
		return ErrCMSMergeOverflow
	}
	merged := make([]uint32, len(c.counter))
	for cell := range merged {
		sum := int64(0)
		for k, src := range sources {
			sum += int64(src.counter[cell]) * weights[k]
		}
		merged[cell] = uint32(sum)
	}
	// The sources may include c itself, so the sums are made before any
	// counter of c is overwritten.
	copy(c.counter, merged)
	c.totalCount = uint64(total)
	return nil
}

// mulInt64 and addInt64 are a*b and a+b, and false where they would
// overflow, as __builtin_mul_overflow and __builtin_add_overflow report.
func mulInt64(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	p := a * b
	if p/b != a || (a == -1 && b == math.MinInt64) || (b == -1 && a == math.MinInt64) {
		return 0, false
	}
	return p, true
}

func addInt64(a, b int64) (int64, bool) {
	s := a + b
	if (b > 0 && s < a) || (b < 0 && s > a) {
		return 0, false
	}
	return s, true
}

// Count estimates how often an item has been seen: never less than the truth,
// and rarely much more.
func (c *CMS) Count(item string) uint32 {
	estimate := uint32(math.MaxUint32)
	for row := uint32(0); row < c.depth; row++ {
		if v := c.counter[c.cell(item, row)]; v < estimate {
			estimate = v
		}
	}
	return estimate
}

// Width, Depth and TotalCount describe the sketch.
func (c *CMS) Width() uint32      { return c.width }
func (c *CMS) Depth() uint32      { return c.depth }
func (c *CMS) TotalCount() uint64 { return c.totalCount }

// MemUsage estimates the bytes held. The counter table is fixed at creation, so
// this never changes for a given sketch.
func (c *CMS) MemUsage() uint64 { return CMSMemUsageFor(c.width, c.depth) }

// CMSMemUsageFor reports what a sketch of these dimensions would cost without
// building one, so that MORRIS.INFO can quote the comparison it is making
// without allocating the megabytes it is quoting.
func CMSMemUsageFor(w uint32, d uint32) uint64 {
	// Two 32-bit dimensions multiply to at most 2^64 - 2^33 + 1 cells, which
	// fits, but four bytes each does not; a product that would wrap is
	// reported as the largest value, which every caller's check refuses.
	cells := uint64(w) * uint64(d)
	if cells > (math.MaxUint64-cmsBaseBytes)/4 {
		return math.MaxUint64
	}
	return cmsBaseBytes + cells*4
}
