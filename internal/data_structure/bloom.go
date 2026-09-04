package data_structure

import (
	"math"

	"github.com/spaolacci/murmur3"
)

// Bloom is a Bloom filter: a bit array and a handful of hash functions that
// together answer "have I seen this?" with no false negatives and a false
// positive rate chosen at creation.
//
// Sizing follows the standard analysis (Bloom, 1970). For n items and a target
// false positive rate p, the optimal number of bits per item is
//
//	-ln(p) / ln(2)^2
//
// and the optimal number of hash functions is that figure times ln(2). The bit
// array is rounded up to whole 64-bit words.
//
// The k hash positions are derived from two 64-bit hashes of the item as
// h1 + i*h2, which Kirsch and Mitzenmacher showed loses nothing against k
// independent hashes. The two come from one 128-bit MurmurHash3 of the item.
type Bloom struct {
	// Hashes is how many positions each item sets.
	Hashes int
	// Entries is the number of items the filter was sized for.
	Entries uint64
	// Error is the false positive rate it was sized for.
	Error float64

	bitPerEntry float64
	bf          []uint8
	// bits is the length of bf in bits and bytes its length in bytes; both are
	// kept because every lookup needs the first and every size report the
	// second.
	bits  uint64
	bytes uint64
}

// ABigSeed seeds the MurmurHash3 that every filter and sketch here hashes with.
// It is fixed because a filter's bits only mean something under the hash that
// set them: a filter written to the append-only file by an earlier build has to
// answer the same way when it is read back.
const ABigSeed uint32 = 0x9747b28c

// bloomHash is the pair of 64-bit hashes an item's positions are derived from.
type bloomHash struct {
	a, b uint64
}

// hashItem computes the pair for an item. It does not depend on the filter, so
// one computation serves every filter in a scalable chain.
func hashItem(item string) bloomHash {
	// The streaming form rather than Sum128WithSeed, for the reason given in
	// cms.go: the one-shot functions trip the race detector's checkptr.
	h := murmur3.New128WithSeed(ABigSeed)
	h.Write([]byte(item))
	a, b := h.Sum128()
	return bloomHash{a: a, b: b}
}

// bitsPerEntry is the optimal density for a false positive rate.
func bitsPerEntry(errorRate float64) float64 {
	return -math.Log(errorRate) / (math.Ln2 * math.Ln2)
}

// CreateBloomFilter sizes a filter for entries items at errorRate false
// positives. The rate must lie strictly between 0 and 1 and entries must be
// positive; callers check that, since the right complaint depends on who asked.
func CreateBloomFilter(entries uint64, errorRate float64) *Bloom {
	b := &Bloom{Entries: entries, Error: errorRate}
	b.bitPerEntry = bitsPerEntry(errorRate)

	wanted := uint64(float64(entries) * b.bitPerEntry)
	words := (wanted + 63) / 64
	b.bytes = words * 8
	b.bits = b.bytes * 8
	b.Hashes = int(math.Ceil(math.Ln2 * b.bitPerEntry))
	b.bf = make([]uint8, b.bytes)
	return b
}

// position is the bit the i-th hash function selects for h.
func (b *Bloom) position(h bloomHash, i int) (byteIndex uint64, mask uint8) {
	bit := (h.a + h.b*uint64(i)) % b.bits
	return bit >> 3, 1 << (bit & 7)
}

// addHash sets the positions for an already-hashed item.
func (b *Bloom) addHash(h bloomHash) {
	for i := 0; i < b.Hashes; i++ {
		idx, mask := b.position(h, i)
		b.bf[idx] |= mask
	}
}

// hasHash reports whether every position for an already-hashed item is set.
func (b *Bloom) hasHash(h bloomHash) bool {
	for i := 0; i < b.Hashes; i++ {
		idx, mask := b.position(h, i)
		if b.bf[idx]&mask == 0 {
			return false
		}
	}
	return true
}

// Add records an item.
func (b *Bloom) Add(item string) { b.addHash(hashItem(item)) }

// Exists reports whether an item may have been added: false is certain, true
// is probable.
func (b *Bloom) Exists(item string) bool { return b.hasHash(hashItem(item)) }
