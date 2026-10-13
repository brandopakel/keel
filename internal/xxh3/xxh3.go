// Package xxh3 computes XXH3's 64-bit hash with its default secret and seed
// 0 (XXH3_64bits), which Redis 8 uses for DIGEST and for SET's IFDEQ and
// IFDNE conditions (t_string.c, stringDigest).
//
// It is translated from the scalar code paths of xxhash.h in xxHash 0.8.3,
// the version Redis 8.10.1 vendors in deps/xxhash, and is distributed under
// that file's licence, which follows:
//
//	xxHash - Extremely Fast Hash algorithm
//	Copyright (C) 2012-2023 Yann Collet
//
//	BSD 2-Clause License (https://www.opensource.org/licenses/bsd-license.php)
//
//	Redistribution and use in source and binary forms, with or without
//	modification, are permitted provided that the following conditions are
//	met:
//
//	   * Redistributions of source code must retain the above copyright
//	     notice, this list of conditions and the following disclaimer.
//	   * Redistributions in binary form must reproduce the above
//	     copyright notice, this list of conditions and the following disclaimer
//	     in the documentation and/or other materials provided with the
//	     distribution.
//
//	THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
//	"AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
//	LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
//	A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
//	OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
//	SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
//	LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
//	DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
//	THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
//	(INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
//	OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
//
//	You can contact the author at:
//	  - xxHash homepage: https://www.xxhash.com
//	  - xxHash source repository: https://github.com/Cyan4973/xxHash
//
// The translation keeps xxhash.h's structure and names, so each function
// can be read beside the one it comes from: XXH3_len_0to16_64b and the
// three below it, XXH3_len_17to128_64b, XXH3_len_129to240_64b, and
// XXH3_hashLong_64b_internal with the scalar accumulate and scramble. The
// seed is always 0, so the seed's terms are left out.
package xxh3

import "math/bits"

const (
	prime32_1 = 0x9E3779B1
	prime32_2 = 0x85EBCA77
	prime32_3 = 0xC2B2AE3D
	prime64_1 = 0x9E3779B185EBCA87
	prime64_2 = 0xC2B2AE3D27D4EB4F
	prime64_3 = 0x165667B19E3779F9
	prime64_4 = 0x85EBCA77C2B2AE63
	prime64_5 = 0x27D4EB2F165667C5
	primeMX1  = 0x165667919E3779F9
	primeMX2  = 0x9FB21C651E98DF25

	stripeLen            = 64
	secretConsumeRate    = 8
	secretSizeMin        = 136
	midsizeMax           = 240
	midsizeStartOffset   = 3
	midsizeLastOffset    = 17
	secretLastAccStart   = 7
	secretMergeAccsStart = 11
)

// kSecret is XXH3's default secret, "taken directly from FARSH".
var kSecret = [192]byte{
	0xb8, 0xfe, 0x6c, 0x39, 0x23, 0xa4, 0x4b, 0xbe, 0x7c, 0x01, 0x81, 0x2c, 0xf7, 0x21, 0xad, 0x1c,
	0xde, 0xd4, 0x6d, 0xe9, 0x83, 0x90, 0x97, 0xdb, 0x72, 0x40, 0xa4, 0xa4, 0xb7, 0xb3, 0x67, 0x1f,
	0xcb, 0x79, 0xe6, 0x4e, 0xcc, 0xc0, 0xe5, 0x78, 0x82, 0x5a, 0xd0, 0x7d, 0xcc, 0xff, 0x72, 0x21,
	0xb8, 0x08, 0x46, 0x74, 0xf7, 0x43, 0x24, 0x8e, 0xe0, 0x35, 0x90, 0xe6, 0x81, 0x3a, 0x26, 0x4c,
	0x3c, 0x28, 0x52, 0xbb, 0x91, 0xc3, 0x00, 0xcb, 0x88, 0xd0, 0x65, 0x8b, 0x1b, 0x53, 0x2e, 0xa3,
	0x71, 0x64, 0x48, 0x97, 0xa2, 0x0d, 0xf9, 0x4e, 0x38, 0x19, 0xef, 0x46, 0xa9, 0xde, 0xac, 0xd8,
	0xa8, 0xfa, 0x76, 0x3f, 0xe3, 0x9c, 0x34, 0x3f, 0xf9, 0xdc, 0xbb, 0xc7, 0xc7, 0x0b, 0x4f, 0x1d,
	0x8a, 0x51, 0xe0, 0x4b, 0xcd, 0xb4, 0x59, 0x31, 0xc8, 0x9f, 0x7e, 0xc9, 0xd9, 0x78, 0x73, 0x64,
	0xea, 0xc5, 0xac, 0x83, 0x34, 0xd3, 0xeb, 0xc3, 0xc5, 0x81, 0xa0, 0xff, 0xfa, 0x13, 0x63, 0xeb,
	0x17, 0x0d, 0xdd, 0x51, 0xb7, 0xf0, 0xda, 0x49, 0xd3, 0x16, 0x55, 0x26, 0x29, 0xd4, 0x68, 0x9e,
	0x2b, 0x16, 0xbe, 0x58, 0x7d, 0x47, 0xa1, 0xfc, 0x8f, 0xf8, 0xb8, 0xd1, 0x7a, 0xd0, 0x31, 0xce,
	0x45, 0xcb, 0x3a, 0x8f, 0x95, 0x16, 0x04, 0x28, 0xaf, 0xd7, 0xfb, 0xca, 0xbb, 0x4b, 0x40, 0x7e,
}

// Hash is XXH3_64bits(s, len(s)).
func Hash(s string) uint64 {
	n := len(s)
	switch {
	case n <= 16:
		return len0to16(s)
	case n <= 128:
		return len17to128(s)
	case n <= midsizeMax:
		return len129to240(s)
	}
	return hashLong(s)
}

// readLE64 and readLE32 read s little-endian from i, as XXH_readLE64 and
// XXH_readLE32 read.
func readLE64(s string, i int) uint64 {
	_ = s[i+7]
	return uint64(s[i]) | uint64(s[i+1])<<8 | uint64(s[i+2])<<16 | uint64(s[i+3])<<24 |
		uint64(s[i+4])<<32 | uint64(s[i+5])<<40 | uint64(s[i+6])<<48 | uint64(s[i+7])<<56
}

func readLE32(s string, i int) uint32 {
	_ = s[i+3]
	return uint32(s[i]) | uint32(s[i+1])<<8 | uint32(s[i+2])<<16 | uint32(s[i+3])<<24
}

// key64 and key32 read the secret at i.
func key64(i int) uint64 {
	k := kSecret[i : i+8]
	return uint64(k[0]) | uint64(k[1])<<8 | uint64(k[2])<<16 | uint64(k[3])<<24 |
		uint64(k[4])<<32 | uint64(k[5])<<40 | uint64(k[6])<<48 | uint64(k[7])<<56
}

func key32(i int) uint32 {
	k := kSecret[i : i+4]
	return uint32(k[0]) | uint32(k[1])<<8 | uint32(k[2])<<16 | uint32(k[3])<<24
}

// mul128Fold64 is XXH3_mul128_fold64: the 128-bit product's halves xored.
func mul128Fold64(a, b uint64) uint64 {
	hi, lo := bits.Mul64(a, b)
	return hi ^ lo
}

// xxh64Avalanche is XXH64_avalanche.
func xxh64Avalanche(h uint64) uint64 {
	h ^= h >> 33
	h *= prime64_2
	h ^= h >> 29
	h *= prime64_3
	h ^= h >> 32
	return h
}

// avalanche is XXH3_avalanche.
func avalanche(h uint64) uint64 {
	h ^= h >> 37
	h *= primeMX1
	h ^= h >> 32
	return h
}

// rrmxmx is XXH3_rrmxmx.
func rrmxmx(h, n uint64) uint64 {
	h ^= bits.RotateLeft64(h, 49) ^ bits.RotateLeft64(h, 24)
	h *= primeMX2
	h ^= (h >> 35) + n
	h *= primeMX2
	return h ^ (h >> 28)
}

// len0to16 is XXH3_len_0to16_64b with its three cases inlined.
func len0to16(s string) uint64 {
	n := len(s)
	switch {
	case n > 8: // XXH3_len_9to16_64b
		bitflip1 := key64(24) ^ key64(32)
		bitflip2 := key64(40) ^ key64(48)
		lo := readLE64(s, 0) ^ bitflip1
		hi := readLE64(s, n-8) ^ bitflip2
		acc := uint64(n) + bits.ReverseBytes64(lo) + hi + mul128Fold64(lo, hi)
		return avalanche(acc)
	case n >= 4: // XXH3_len_4to8_64b
		input1 := readLE32(s, 0)
		input2 := readLE32(s, n-4)
		bitflip := key64(8) ^ key64(16)
		input64 := uint64(input2) + uint64(input1)<<32
		return rrmxmx(input64^bitflip, uint64(n))
	case n > 0: // XXH3_len_1to3_64b
		c1, c2, c3 := uint32(s[0]), uint32(s[n>>1]), uint32(s[n-1])
		combined := c1<<16 | c2<<24 | c3 | uint32(n)<<8
		bitflip := uint64(key32(0) ^ key32(4))
		return xxh64Avalanche(uint64(combined) ^ bitflip)
	}
	return xxh64Avalanche(key64(56) ^ key64(64))
}

// mix16 is XXH3_mix16B: the 16 bytes of s at i, keyed with the secret at k.
func mix16(s string, i, k int) uint64 {
	return mul128Fold64(readLE64(s, i)^key64(k), readLE64(s, i+8)^key64(k+8))
}

// len17to128 is XXH3_len_17to128_64b.
func len17to128(s string) uint64 {
	n := len(s)
	acc := uint64(n) * prime64_1
	if n > 32 {
		if n > 64 {
			if n > 96 {
				acc += mix16(s, 48, 96)
				acc += mix16(s, n-64, 112)
			}
			acc += mix16(s, 32, 64)
			acc += mix16(s, n-48, 80)
		}
		acc += mix16(s, 16, 32)
		acc += mix16(s, n-32, 48)
	}
	acc += mix16(s, 0, 0)
	acc += mix16(s, n-16, 16)
	return avalanche(acc)
}

// len129to240 is XXH3_len_129to240_64b.
func len129to240(s string) uint64 {
	n := len(s)
	acc := uint64(n) * prime64_1
	rounds := n / 16
	for i := 0; i < 8; i++ {
		acc += mix16(s, 16*i, 16*i)
	}
	accEnd := mix16(s, n-16, secretSizeMin-midsizeLastOffset)
	acc = avalanche(acc)
	for i := 8; i < rounds; i++ {
		accEnd += mix16(s, 16*i, 16*(i-8)+midsizeStartOffset)
	}
	return avalanche(acc + accEnd)
}

// hashLong is XXH3_hashLong_64b_internal with the default secret: blocks of
// stripes accumulated into eight lanes, scrambled after each block, the
// last stripe accumulated again from the input's end, and the lanes merged.
func hashLong(s string) uint64 {
	acc := [8]uint64{prime32_3, prime64_1, prime64_2, prime64_3, prime64_4, prime32_2, prime64_5, prime32_1}
	n := len(s)
	stripesPerBlock := (len(kSecret) - stripeLen) / secretConsumeRate
	blockLen := stripeLen * stripesPerBlock
	blocks := (n - 1) / blockLen
	for b := 0; b < blocks; b++ {
		accumulate(&acc, s, b*blockLen, stripesPerBlock)
		scramble(&acc, len(kSecret)-stripeLen)
	}
	stripes := ((n - 1) - blockLen*blocks) / stripeLen
	accumulate(&acc, s, blocks*blockLen, stripes)
	accumulate512(&acc, s, n-stripeLen, len(kSecret)-stripeLen-secretLastAccStart)

	// XXH3_finalizeLong_64b and XXH3_mergeAccs.
	result := uint64(n) * prime64_1
	for i := 0; i < 4; i++ {
		k := secretMergeAccsStart + 16*i
		result += mul128Fold64(acc[2*i]^key64(k), acc[2*i+1]^key64(k+8))
	}
	return avalanche(result)
}

// accumulate is XXH3_accumulate: stripes stripes of s from off, each keyed
// with the secret eight bytes further on.
func accumulate(acc *[8]uint64, s string, off, stripes int) {
	for i := 0; i < stripes; i++ {
		accumulate512(acc, s, off+i*stripeLen, i*secretConsumeRate)
	}
}

// accumulate512 is XXH3_accumulate_512_scalar: one 64-byte stripe of s at
// in, keyed with the secret at k.
func accumulate512(acc *[8]uint64, s string, in, k int) {
	for lane := 0; lane < 8; lane++ {
		value := readLE64(s, in+8*lane)
		key := value ^ key64(k+8*lane)
		acc[lane^1] += value
		acc[lane] += uint64(uint32(key)) * (key >> 32)
	}
}

// scramble is XXH3_scrambleAcc_scalar, with the secret at k.
func scramble(acc *[8]uint64, k int) {
	for lane := 0; lane < 8; lane++ {
		a := acc[lane]
		a ^= a >> 47
		a ^= key64(k + 8*lane)
		a *= prime32_1
		acc[lane] = a
	}
}
