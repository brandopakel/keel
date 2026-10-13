package xxh3

import (
	"fmt"
	"testing"
)

// sanityBuffer is the input xxHash's own sanity check hashes prefixes of:
// byte i is the top byte of a 64-bit generator that starts at xxHash's
// PRIME32 and is multiplied by its PRIME64 after every byte.
func sanityBuffer(n int) string {
	const prime32, prime64 = 2654435761, 11400714785074694797
	b := make([]byte, n)
	gen := uint64(prime32)
	for i := range b {
		b[i] = byte(gen >> 56)
		gen *= prime64
	}
	return string(b)
}

// TestHashMatchesXXHashTestVectors holds Hash to the XXH3_64bits results
// xxHash 0.8.3 publishes for seed 0 (cli/xsum_sanity_check.c,
// XSUM_XXH3_testdata): one length in each of the algorithm's ranges, and
// the long-input cases that end at a stripe, a block, or partway through
// one.
func TestHashMatchesXXHashTestVectors(t *testing.T) {
	t.Parallel()
	buffer := sanityBuffer(2367)
	for _, tc := range []struct {
		n    int
		want uint64
	}{
		{0, 0x2D06800538D394C2},
		{1, 0xC44BDFF4074EECDB},
		{6, 0x27B56A84CD2D7325},
		{12, 0xA713DAF0DFBB77E7},
		{24, 0xA3FE70BF9D3510EB},
		{48, 0x397DA259ECBA1F11},
		{80, 0xBCDEFBBB2C47C90A},
		{195, 0xCD94217EE362EC3A},
		{403, 0xCDEB804D65C6DEA4},
		{512, 0x617E49599013CB6B},
		{2048, 0xDD59E2C3A5F038E0},
		{2099, 0xC6B9D9B3FC9AC765},
		{2240, 0x6E73A90539CF2948},
		{2367, 0xCB37AEB9E5D361ED},
	} {
		if got := Hash(buffer[:tc.n]); got != tc.want {
			t.Errorf("Hash of %d bytes: %016x, want %016x", tc.n, got, tc.want)
		}
	}
}

// TestHashReadsOnlyItsInput hashes a prefix of a longer string at every
// length the ranges meet at, so a read past the input's end would change
// the result between the prefix and a copy of it.
func TestHashReadsOnlyItsInput(t *testing.T) {
	t.Parallel()
	buffer := sanityBuffer(4096)
	for _, n := range []int{0, 1, 2, 3, 4, 8, 9, 16, 17, 32, 33, 64, 65, 96, 97, 128, 129, 239, 240, 241, 1024, 1025, 2047, 2049} {
		prefix := buffer[:n]
		copied := string([]byte(prefix))
		if Hash(prefix) != Hash(copied) {
			t.Errorf("%d bytes: the prefix and its copy hash differently", n)
		}
	}
}

func TestHashAllocatesNothing(t *testing.T) {
	buffer := sanityBuffer(4096)
	if allocs := testing.AllocsPerRun(100, func() { _ = Hash(buffer) }); allocs != 0 {
		t.Fatalf("Hash allocated %v times per call", allocs)
	}
}

func BenchmarkHash(b *testing.B) {
	buffer := sanityBuffer(1 << 16)
	for _, n := range []int{8, 64, 200, 1024, 1 << 16} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			b.SetBytes(int64(n))
			for i := 0; i < b.N; i++ {
				_ = Hash(buffer[:n])
			}
		})
	}
}
