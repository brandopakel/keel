package data_structure

import (
	"fmt"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBloomNeverForgetsWhatItWasGiven(t *testing.T) {
	b := CreateBloomFilter(1000, 0.01)
	for i := 0; i < 1000; i++ {
		b.Add(fmt.Sprintf("item-%d", i))
	}
	for i := 0; i < 1000; i++ {
		assert.True(t, b.Exists(fmt.Sprintf("item-%d", i)), "a Bloom filter has no false negatives")
	}
}

func TestBloomFalsePositivesStayNearTheTarget(t *testing.T) {
	for _, target := range []float64{0.1, 0.01, 0.001} {
		b := CreateBloomFilter(10000, target)
		for i := 0; i < 10000; i++ {
			b.Add(fmt.Sprintf("in-%d", i))
		}
		hits := 0
		const probes = 100000
		for i := 0; i < probes; i++ {
			if b.Exists(fmt.Sprintf("out-%d", i)) {
				hits++
			}
		}
		rate := float64(hits) / probes
		// The analysis gives the rate at exactly the sized number of items;
		// twice the target is a generous ceiling and a tenth a floor that
		// catches a filter far larger than asked for.
		assert.Less(t, rate, target*2, "target %g", target)
		assert.Greater(t, rate, target/10, "target %g", target)
	}
}

func TestBloomSizing(t *testing.T) {
	b := CreateBloomFilter(1000, 0.01)
	assert.EqualValues(t, 1000, b.Entries)
	assert.EqualValues(t, 0.01, b.Error)
	// -ln(0.01)/ln(2)^2 is about 9.585 bits per item and 7 hash functions.
	assert.InDelta(t, 9.585, b.bitPerEntry, 0.001)
	assert.Equal(t, 7, b.Hashes)
	assert.Zero(t, b.bits%64, "the bit array is whole words")
	assert.Equal(t, b.bytes*8, b.bits)
	assert.EqualValues(t, len(b.bf), b.bytes)
	assert.GreaterOrEqual(t, float64(b.bits), 1000*b.bitPerEntry)
	assert.Less(t, float64(b.bits), 1000*b.bitPerEntry+64)

	one := CreateBloomFilter(1, 0.5)
	assert.EqualValues(t, 64, one.bits, "even the smallest filter is one word")
	assert.GreaterOrEqual(t, one.Hashes, 1)
}

func TestBloomHashIsStable(t *testing.T) {
	// Filters written by earlier builds were hashed with these exact values,
	// so they are pinned: a change here would make a restored filter answer
	// differently about the items it holds.
	h := hashItem("abcdef")
	assert.Equal(t, hashItem("abcdef"), h)
	assert.NotEqual(t, hashItem("abcdeg"), h)
	assert.EqualValues(t, 0x9747b28c, ABigSeed)

	b := CreateBloomFilter(10, 0.01)
	b.addHash(h)
	assert.True(t, b.hasHash(h))
	assert.True(t, b.Exists("abcdef"), "adding by hash and asking by item agree")
}

func TestBloomPositionsStayInsideTheArray(t *testing.T) {
	b := CreateBloomFilter(3, 0.3)
	for i := 0; i < 10000; i++ {
		h := hashItem(fmt.Sprintf("%d", i))
		for k := 0; k < b.Hashes; k++ {
			idx, mask := b.position(h, k)
			assert.Less(t, idx, uint64(len(b.bf)))
			assert.NotZero(t, mask)
		}
	}
	assert.False(t, math.IsNaN(b.bitPerEntry))
}
