package data_structure

import (
	"github.com/spaolacci/murmur3"
)

// Implementation of the Cuckoo filter
// https://www.cs.cmu.edu/~dga/papers/cuckoo-conext2014.pdf
//
// Like a Bloom filter it answers "have I seen this?" with false positives but
// no false negatives, in far less space than storing the items. Unlike a Bloom
// filter it supports deletion, which is the reason to reach for it: a Bloom
// filter's bits are shared between items, so clearing them for one item would
// erase evidence of others.
//
// It works by storing a short fingerprint of each item in a hash table with two
// candidate buckets per item, and resolving collisions by evicting an existing
// fingerprint to its own alternate bucket - the cuckoo, pushing the other egg
// out of the nest. The difficulty is that eviction has to find an evicted
// fingerprint's other bucket without knowing the item it came from, since the
// item was never stored. Partial-key cuckoo hashing solves it:
//
//	i2 = i1 XOR hash(fingerprint)
//
// XOR is its own inverse, so the relation reads both ways: from either bucket
// and the fingerprint alone, the other bucket falls out. That is what makes the
// structure possible at all.

const (
	// CuckooBucketSize is how many fingerprints share a bucket. Four gives a
	// load factor around 95% before insertion starts failing; two only reaches
	// about 84%, because a fingerprint has fewer places to land.
	CuckooBucketSize = 4
	// CuckooMaxKicks bounds an eviction chain. The paper uses 500; reaching it
	// means the table is effectively full.
	CuckooMaxKicks = 500
	// cuckooLoadFactor is the occupancy the table is sized for.
	cuckooLoadFactor = 0.95
	// CfDefaultCapacity sizes a filter created implicitly by CF.ADD.
	CfDefaultCapacity = 1024
	// emptyFingerprint marks a free slot, which is why a real fingerprint is
	// never allowed to be zero.
	emptyFingerprint = 0
)

// CuckooFilter is a cuckoo filter with 16-bit fingerprints.
//
// The false positive rate is about 2b/2^f - eight chances in 65536, roughly
// 0.012% - since a lookup examines 2 buckets of 4 entries and any of them may
// collide by chance.
type CuckooFilter struct {
	buckets    []uint16
	numBuckets uint64 // always a power of two; see altIndex
	mask       uint64 // numBuckets-1
	inserted   uint64
	deleted    uint64
	capacity   uint64

	// rngState drives the choice of which entry to evict. A per-filter
	// xorshift rather than math/rand keeps a filter's behaviour reproducible
	// and touches no shared state.
	rngState uint64
}

// CreateCuckooFilter sizes a filter to hold about capacity items.
func CreateCuckooFilter(capacity uint64) *CuckooFilter {
	if capacity < 1 {
		capacity = 1
	}
	needed := uint64(float64(capacity)/(CuckooBucketSize*cuckooLoadFactor)) + 1
	numBuckets := nextPowerOfTwo(needed)
	return &CuckooFilter{
		buckets:    make([]uint16, numBuckets*CuckooBucketSize),
		numBuckets: numBuckets,
		mask:       numBuckets - 1,
		capacity:   capacity,
		rngState:   0x9E3779B97F4A7C15,
	}
}

// nextPowerOfTwo rounds up. The table size must be a power of two or the XOR in
// altIndex could produce an index outside the table.
func nextPowerOfTwo(n uint64) uint64 {
	p := uint64(1)
	for p < n {
		p <<= 1
	}
	return p
}

// fingerprintOf derives the stored fingerprint and first bucket from an item.
//
// Zero is reserved for an empty slot, so a fingerprint that hashes to zero is
// nudged to one. That very slightly biases one fingerprint value, which costs a
// negligible amount of false positive rate and buys a free emptiness test.
func (c *CuckooFilter) fingerprintOf(item string) (fp uint16, i1 uint64) {
	h := murmur3.Sum64WithSeed([]byte(item), ABigSeed)
	fp = uint16(h >> 48)
	if fp == emptyFingerprint {
		fp = 1
	}
	return fp, h & c.mask
}

// altIndex returns the other bucket a fingerprint may live in.
//
// The fingerprint is mixed before the XOR rather than used raw. Without mixing,
// a 16-bit fingerprint could only move an index within its low 16 bits, so
// entries in a large table would never reach most of it.
func (c *CuckooFilter) altIndex(fp uint16, i uint64) uint64 {
	x := uint64(fp) * 0x9E3779B97F4A7C15
	x ^= x >> 29
	x *= 0xBF58476D1CE4E5B9
	x ^= x >> 32
	return (i ^ x) & c.mask
}

func (c *CuckooFilter) nextRand() uint64 {
	c.rngState ^= c.rngState << 13
	c.rngState ^= c.rngState >> 7
	c.rngState ^= c.rngState << 17
	return c.rngState
}

// tryInsert puts fp in bucket i if there is a free slot.
func (c *CuckooFilter) tryInsert(fp uint16, i uint64) bool {
	base := i * CuckooBucketSize
	for j := uint64(0); j < CuckooBucketSize; j++ {
		if c.buckets[base+j] == emptyFingerprint {
			c.buckets[base+j] = fp
			return true
		}
	}
	return false
}

func (c *CuckooFilter) bucketContains(fp uint16, i uint64) bool {
	base := i * CuckooBucketSize
	for j := uint64(0); j < CuckooBucketSize; j++ {
		if c.buckets[base+j] == fp {
			return true
		}
	}
	return false
}

// Insert adds item, reporting false if the filter is too full to take it.
//
// Duplicates are allowed: the same item may be inserted several times and
// occupies a slot each time, which is what lets Count report a multiplicity and
// Delete remove one copy at a time.
func (c *CuckooFilter) Insert(item string) bool {
	fp, i1 := c.fingerprintOf(item)
	i2 := c.altIndex(fp, i1)

	if c.tryInsert(fp, i1) || c.tryInsert(fp, i2) {
		c.inserted++
		return true
	}

	// Both candidates are full, so make room by evicting. Each step displaces
	// one fingerprint and tries to rehome it in its own alternate bucket, which
	// may in turn displace another.
	i := i1
	if c.nextRand()&1 == 1 {
		i = i2
	}

	// Every swap is recorded so a failed chain can be undone. Without this, a
	// chain that runs out of kicks leaves the last displaced fingerprint
	// homeless and it is simply dropped - an item that was successfully
	// inserted earlier silently disappears, and the filter starts reporting
	// false negatives. That breaks the one guarantee a filter of this kind
	// makes, and it does so invisibly.
	type kick struct{ bucket, slot uint64 }
	chain := make([]kick, 0, 16)

	for n := 0; n < CuckooMaxKicks; n++ {
		slot := c.nextRand() % CuckooBucketSize
		idx := i*CuckooBucketSize + slot
		fp, c.buckets[idx] = c.buckets[idx], fp
		chain = append(chain, kick{i, slot})

		i = c.altIndex(fp, i)
		if c.tryInsert(fp, i) {
			c.inserted++
			return true
		}
	}

	// Out of kicks. Walk the swaps back so the table is exactly as it was, and
	// fp ends up holding the fingerprint we were originally asked to insert.
	for k := len(chain) - 1; k >= 0; k-- {
		idx := chain[k].bucket*CuckooBucketSize + chain[k].slot
		fp, c.buckets[idx] = c.buckets[idx], fp
	}
	return false
}

// Lookup reports whether item may have been inserted. False positives are
// possible; false negatives are not.
func (c *CuckooFilter) Lookup(item string) bool {
	fp, i1 := c.fingerprintOf(item)
	return c.bucketContains(fp, i1) || c.bucketContains(fp, c.altIndex(fp, i1))
}

// Count reports how many copies of item the filter holds.
func (c *CuckooFilter) Count(item string) uint64 {
	fp, i1 := c.fingerprintOf(item)
	i2 := c.altIndex(fp, i1)

	count := c.countIn(fp, i1)
	if i2 != i1 {
		// A fingerprint whose mixed hash is a multiple of the table size maps
		// both candidates to the same bucket; counting it twice would double.
		count += c.countIn(fp, i2)
	}
	return count
}

func (c *CuckooFilter) countIn(fp uint16, i uint64) uint64 {
	base := i * CuckooBucketSize
	var n uint64
	for j := uint64(0); j < CuckooBucketSize; j++ {
		if c.buckets[base+j] == fp {
			n++
		}
	}
	return n
}

// Delete removes one copy of item and reports whether it found one.
//
// Only delete items that were actually inserted. Two items sharing a
// fingerprint and a bucket are indistinguishable, so deleting one the filter
// never held can remove another's entry and turn it into a false negative. That
// is a property of the structure, not of this implementation.
func (c *CuckooFilter) Delete(item string) bool {
	fp, i1 := c.fingerprintOf(item)
	if c.deleteFrom(fp, i1) {
		return true
	}
	i2 := c.altIndex(fp, i1)
	return i2 != i1 && c.deleteFrom(fp, i2)
}

func (c *CuckooFilter) deleteFrom(fp uint16, i uint64) bool {
	base := i * CuckooBucketSize
	for j := uint64(0); j < CuckooBucketSize; j++ {
		if c.buckets[base+j] == fp {
			c.buckets[base+j] = emptyFingerprint
			c.deleted++
			return true
		}
	}
	return false
}

func (c *CuckooFilter) Size() uint64       { return c.inserted - c.deleted }
func (c *CuckooFilter) Inserted() uint64   { return c.inserted }
func (c *CuckooFilter) Deleted() uint64    { return c.deleted }
func (c *CuckooFilter) Capacity() uint64   { return c.capacity }
func (c *CuckooFilter) NumBuckets() uint64 { return c.numBuckets }
func (c *CuckooFilter) MemUsage() uint64   { return uint64(len(c.buckets)) * 2 }
