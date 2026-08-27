package data_structure

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestAltIndexIsItsOwnInverse pins the property the whole structure rests on.
//
// An evicted fingerprint has to be rehomed, but the item it came from was never
// stored, so its other bucket must be derivable from the fingerprint and the
// current bucket alone. That works only because XOR is an involution: applying
// altIndex twice must return exactly where it started. If it did not, eviction
// would scatter fingerprints into buckets where lookup never searches, and the
// filter would report false negatives.
func TestAltIndexIsItsOwnInverse(t *testing.T) {
	c := CreateCuckooFilter(100000)
	for _, fp := range []uint16{1, 2, 255, 256, 4096, 32768, 65535} {
		for i := uint64(0); i < c.numBuckets; i += 1 + c.numBuckets/512 {
			alt := c.altIndex(fp, i)
			assert.Less(t, alt, c.numBuckets, "alternate index must stay inside the table")
			assert.Equal(t, i, c.altIndex(fp, alt),
				"altIndex must be its own inverse for fp=%d i=%d", fp, i)
		}
	}
}

func TestFingerprintIsNeverZero(t *testing.T) {
	c := CreateCuckooFilter(1000)
	for i := 0; i < 200000; i++ {
		fp, idx := c.fingerprintOf("item:" + strconv.Itoa(i))
		assert.NotEqual(t, uint16(emptyFingerprint), fp, "zero is reserved for an empty slot")
		assert.Less(t, idx, c.numBuckets)
	}
}

func TestNumBucketsIsAPowerOfTwo(t *testing.T) {
	for _, capacity := range []uint64{1, 2, 7, 100, 1000, 65537} {
		c := CreateCuckooFilter(capacity)
		assert.Equal(t, uint64(0), c.numBuckets&(c.numBuckets-1),
			"table size must be a power of two or the XOR can leave the table")
		assert.Equal(t, c.numBuckets-1, c.mask)
	}
}

func TestEmptyFilterFindsNothing(t *testing.T) {
	c := CreateCuckooFilter(1000)
	assert.False(t, c.Lookup("anything"))
	assert.Equal(t, uint64(0), c.Count("anything"))
	assert.False(t, c.Delete("anything"))
}

// TestNoFalseNegatives is the contract. Everything inserted must be found.
func TestNoFalseNegatives(t *testing.T) {
	c := CreateCuckooFilter(10000)
	for i := 0; i < 9000; i++ {
		assert.True(t, c.Insert("item:"+strconv.Itoa(i)), "insert %d failed below capacity", i)
	}
	for i := 0; i < 9000; i++ {
		assert.True(t, c.Lookup("item:"+strconv.Itoa(i)), "item %d went missing", i)
	}
}

// TestFailedInsertLeavesTheFilterIntact is the reason Insert records its
// eviction chain.
//
// When a chain runs out of kicks it is holding a fingerprint that no longer has
// a home. Dropping it loses an item that was already successfully inserted, and
// the filter starts answering "no" for something it holds - a false negative,
// which is the one thing it promises never to do. Nothing about the failing
// insert reveals this; the damage is to a different item entirely.
func TestFailedInsertLeavesTheFilterIntact(t *testing.T) {
	c := CreateCuckooFilter(1000)

	var stored []string
	for i := 0; ; i++ {
		item := "item:" + strconv.Itoa(i)
		if !c.Insert(item) {
			break // the filter is full; this is the path under test
		}
		stored = append(stored, item)
		if i > 200000 {
			t.Fatal("filter never filled up")
		}
	}
	assert.NotEmpty(t, stored)

	missing := 0
	for _, item := range stored {
		if !c.Lookup(item) {
			missing++
		}
	}
	assert.Zero(t, missing,
		"%d of %d inserted items vanished when an insert failed; a failed insert "+
			"must not disturb what is already stored", missing, len(stored))
}

func TestReachesHighLoadFactor(t *testing.T) {
	const capacity = 10000
	c := CreateCuckooFilter(capacity)
	inserted := 0
	for i := 0; ; i++ {
		if !c.Insert("item:" + strconv.Itoa(i)) {
			break
		}
		inserted++
		if i > 200000 {
			break
		}
	}
	slots := float64(c.numBuckets * CuckooBucketSize)
	load := float64(inserted) / slots
	assert.Greater(t, load, 0.90, "should pack to a high load factor, got %.2f%%", load*100)
	assert.GreaterOrEqual(t, uint64(inserted), uint64(capacity),
		"a filter should hold at least the capacity it was asked for")
}

func TestDeleteRemovesItem(t *testing.T) {
	c := CreateCuckooFilter(1000)
	c.Insert("present")
	assert.True(t, c.Lookup("present"))
	assert.True(t, c.Delete("present"))
	assert.False(t, c.Lookup("present"), "a deleted item should be gone")
	assert.False(t, c.Delete("present"), "deleting twice should report nothing removed")
}

// TestDeleteRemovesOneCopyAtATime covers what a Bloom filter cannot express at
// all: multiplicity.
func TestDeleteRemovesOneCopyAtATime(t *testing.T) {
	c := CreateCuckooFilter(1000)
	for i := 0; i < 3; i++ {
		assert.True(t, c.Insert("dup"))
	}
	assert.Equal(t, uint64(3), c.Count("dup"))

	assert.True(t, c.Delete("dup"))
	assert.Equal(t, uint64(2), c.Count("dup"))
	assert.True(t, c.Lookup("dup"), "other copies must survive")

	assert.True(t, c.Delete("dup"))
	assert.True(t, c.Delete("dup"))
	assert.Equal(t, uint64(0), c.Count("dup"))
	assert.False(t, c.Lookup("dup"))
}

// TestDeleteDoesNotDisturbOtherItems checks that removing one item leaves the
// rest of a populated filter findable.
func TestDeleteDoesNotDisturbOtherItems(t *testing.T) {
	c := CreateCuckooFilter(20000)
	const n = 15000
	for i := 0; i < n; i++ {
		c.Insert("item:" + strconv.Itoa(i))
	}
	for i := 0; i < n; i += 2 {
		assert.True(t, c.Delete("item:"+strconv.Itoa(i)), "delete %d", i)
	}
	for i := 1; i < n; i += 2 {
		assert.True(t, c.Lookup("item:"+strconv.Itoa(i)),
			"item %d was lost while deleting its neighbours", i)
	}
}

// TestFalsePositiveRate checks the accuracy claim. With 16-bit fingerprints and
// four entries per bucket the expected rate is about 2b/2^f, roughly 0.012%.
func TestFalsePositiveRate(t *testing.T) {
	c := CreateCuckooFilter(50000)
	const inserted = 45000
	for i := 0; i < inserted; i++ {
		c.Insert("present:" + strconv.Itoa(i))
	}

	const probes = 200000
	falsePositives := 0
	for i := 0; i < probes; i++ {
		if c.Lookup("absent:" + strconv.Itoa(i)) {
			falsePositives++
		}
	}
	rate := float64(falsePositives) / probes
	assert.Less(t, rate, 0.002,
		"false positive rate %.4f%% is far above the expected ~0.012%%", rate*100)
}

func TestSizeTracksInsertsAndDeletes(t *testing.T) {
	c := CreateCuckooFilter(1000)
	for i := 0; i < 100; i++ {
		c.Insert("item:" + strconv.Itoa(i))
	}
	assert.Equal(t, uint64(100), c.Size())
	for i := 0; i < 30; i++ {
		c.Delete("item:" + strconv.Itoa(i))
	}
	assert.Equal(t, uint64(70), c.Size())
	assert.Equal(t, uint64(100), c.Inserted())
	assert.Equal(t, uint64(30), c.Deleted())
}
