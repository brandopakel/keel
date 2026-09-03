package data_structure

import (
	"math"

	"github.com/brandopakel/keel/internal/config"
)

// Approximate LFU.
//
// LRU keeps what was read recently; LFU keeps what is read often. The
// difference shows up under a scan: reading a million keys once each is, to
// LRU, a million very recent keys, and it will evict a genuinely hot working
// set to make room for data nothing will ask for again. LFU is not fooled,
// because being read once is not being read often.
//
// Counting accesses per key naively fails twice, and Redis's answers to both
// are what make the policy practical.
//
// A plain counter needs enough width to hold a popular key's access count, and
// that width is paid on every key. So the counter is logarithmic: incremented
// only with probability 1/(base*factor+1), so it rises quickly at first and
// then ever more slowly. Eight bits then span one access to millions.
//
// A counter that only rises never forgets. A key hammered yesterday and
// untouched since would outrank one in active use, and the cache would fill
// with history. So the counter decays, one point per elapsed period, applied
// lazily when the key is next looked at rather than by sweeping the keyspace.
//
// Both halves live in the same 64-bit access word the LRU policy uses for a
// clock reading: a decay timestamp in the high 56 bits, the counter in the low
// 8. One overloaded word rather than three honest fields because the cost is
// per key, and so is multiplied by the size of the keyspace.

const (
	// lfuInitVal is the counter a new key starts with.
	lfuInitVal = 5
	// lfuMaxVal is what fits in the eight bits reserved for the counter.
	lfuMaxVal = math.MaxUint8
)

func packLFU(decayAt uint64, freq uint8) uint64 { return decayAt<<8 | uint64(freq) }
func lfuFreqOf(access uint64) uint8             { return uint8(access) }
func lfuDecayAtOf(access uint64) uint64         { return access >> 8 }

// touchLFU applies any decay owed and then attempts to increment.
func touchLFU(access *uint64) {
	freq := decayedFreq(*access)
	*access = packLFU(evictionClock, lfuLogIncr(freq))
}

// decayedFreq reads the counter with any owed decay applied, without writing.
//
// Decay is lazy. Sweeping the keyspace once per period would be O(n) work at
// intervals, for keys that may never be looked at again; computing it from the
// stored timestamp costs nothing until someone asks.
func decayedFreq(access uint64) uint8 {
	freq := lfuFreqOf(access)
	period := uint64(config.LFUDecayPeriod)
	if period == 0 || freq == 0 {
		return freq
	}

	periods := (evictionClock - lfuDecayAtOf(access)) / period
	switch {
	case periods == 0:
		return freq
	case periods >= uint64(freq):
		return 0
	default:
		return freq - uint8(periods)
	}
}

// lfuLogIncr raises the counter with probability 1/(base*factor+1).
//
// The probability falls as the counter rises, so early accesses move it and
// later ones mostly do not. That is what lets eight bits distinguish a key read
// ten times from one read ten million times, at the cost of the value being a
// rank rather than a count - which is all eviction needs.
func lfuLogIncr(freq uint8) uint8 {
	if freq >= lfuMaxVal {
		return lfuMaxVal
	}
	base := float64(freq) - lfuInitVal
	if base < 0 {
		base = 0
	}
	p := 1.0 / (base*float64(config.LFULogFactor) + 1)
	if evictionRandFloat() < p {
		return freq + 1
	}
	return freq
}

func evictionRandFloat() float64 {
	return float64(nextEvictionRand()>>11) / float64(uint64(1)<<53)
}
