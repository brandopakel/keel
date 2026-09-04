package data_structure

import (
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
// of the total count with probability 1 - errProb.
func CalcCMSDim(errRate float64, errProb float64) (width, depth uint32) {
	width = uint32(math.Ceil(2.0 / errRate))
	depth = uint32(math.Ceil(math.Log2(1.0 / errProb)))
	return width, depth
}

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

// IncrBy adds value to an item's count and returns its new estimate, and
// whether any of the item's counters had to stop at the maximum to take it. A
// counter that would overflow saturates there rather than wrapping; the
// estimate is then the maximum, but so is the estimate of an item that was
// incremented to exactly that, which is why the second result exists.
func (c *CMS) IncrBy(item string, value uint32) (estimate uint32, saturated bool) {
	estimate = math.MaxUint32
	for row := uint32(0); row < c.depth; row++ {
		at := c.cell(item, row)
		if math.MaxUint32-c.counter[at] < value {
			c.counter[at] = math.MaxUint32
			saturated = true
		} else {
			c.counter[at] += value
		}
		if c.counter[at] < estimate {
			estimate = c.counter[at]
		}
	}
	c.totalCount += uint64(value)
	return estimate, saturated
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
