package data_structure

import (
	"math"

	"memkv/internal/config"
)

// Approximate LFU eviction.
//
// LRU keeps what was read recently; LFU keeps what is read often. The
// difference shows up under a scan: reading a million keys once each is, to
// LRU, a million very recent keys, and it will evict a genuinely hot working
// set to make room for data nothing will ask for again. LFU is not fooled,
// because being read once is not being read often.
//
// Counting accesses per key naively has two problems, and Redis's answers to
// both are what make the policy practical.
//
// A plain counter needs enough width to hold a popular key's access count, and
// that width is paid on every key in the keyspace. So the counter is
// logarithmic: it is incremented only with probability 1/(base*factor+1), so it
// rises quickly at first and then ever more slowly. Eight bits then span a
// range from one access to millions.
//
// A counter that only rises never forgets. A key hammered yesterday and
// untouched since would outrank one in active use, and the cache would fill
// with history. So the counter decays: one point per elapsed decay period,
// applied lazily when the key is next looked at rather than by sweeping the
// keyspace.

const (
	// lfuInitVal is the counter a new key starts with. New keys need credit or
	// they are, by definition, the least frequently used thing in the
	// dictionary and get evicted before they can prove otherwise.
	lfuInitVal = 5
	// lfuMaxVal is what fits in the eight bits reserved for the counter.
	lfuMaxVal = math.MaxUint8
)

// touchLFU applies any decay owed and then attempts to increment.
func (d *Dict) touchLFU(obj *Obj) {
	freq := d.lfuDecayedFreq(obj)
	obj.setLFU(d.clock, d.lfuLogIncr(freq))
}

// lfuDecayedFreq reads the counter with any owed decay applied, without
// modifying the object.
//
// Decay is lazy. Sweeping the keyspace once per period would be O(n) work at
// intervals, for keys that may never be looked at again; computing it from the
// stored timestamp costs nothing until someone asks.
func (d *Dict) lfuDecayedFreq(obj *Obj) uint8 {
	freq := obj.lfuFreq()
	period := uint64(config.LFUDecayPeriod)
	if period == 0 || freq == 0 {
		return freq
	}

	elapsed := d.clock - obj.lfuDecayAt()
	periods := elapsed / period
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
// ten times from one read ten million times, at the cost of the counter being a
// rank rather than a count - the value is not the number of accesses, only
// monotonic in it, which is all eviction needs.
func (d *Dict) lfuLogIncr(freq uint8) uint8 {
	if freq >= lfuMaxVal {
		return lfuMaxVal
	}
	base := float64(freq) - lfuInitVal
	if base < 0 {
		base = 0
	}
	p := 1.0 / (base*float64(config.LFULogFactor) + 1)
	if d.randFloat() < p {
		return freq + 1
	}
	return freq
}

// randFloat returns a value in [0,1) from the dictionary's own generator.
func (d *Dict) randFloat() float64 {
	return float64(d.nextRand()>>11) / float64(uint64(1)<<53)
}

// evictApproxLFU removes one key, approximately the least frequently used.
//
// The score is the decayed counter, so the lowest score is the key with the
// weakest recent access history.
func (d *Dict) evictApproxLFU() {
	d.evictBySampling(func(obj *Obj) uint64 { return uint64(d.lfuDecayedFreq(obj)) })
}
