package data_structure

import (
	"memkv/internal/config"
)

// Memory accounting, so the keyspace can be bounded by bytes rather than by
// key count.
//
// A key limit treats an 8-byte value and an 8MB value as equally expensive,
// which is the wrong question for a cache: what runs out is memory. Bounding by
// bytes means knowing what a key costs, and Go gives no way to ask the
// allocator, so the cost is estimated.
//
// The estimate is calibrated against real heap growth rather than derived from
// struct sizes. Adding 200,000 entries and measuring HeapAlloc either side:
//
//	key len   value len   bytes/entry   minus key+value
//	     16           8         115.1              91.1
//	     16          64         179.0              99.0
//	     32          64         194.9              98.9
//	     64          64         226.9              98.9
//	    128          64         290.9              98.9
//
// Key and value bytes are charged one for one, and everything else - the map
// bucket slot, the string headers, the Obj, the pointer - comes to a constant
// just under 100 bytes.
const entryOverhead = 100

// expiryOverhead is the additional cost of a key with a TTL, which lives in a
// second map keyed by object pointer.
const expiryOverhead = 48

// entryBytes estimates what one key costs.
//
// It ignores the allocator rounding each allocation up to a size class, since
// modelling that would mean hard-coding a table the runtime is free to change.
// Checked against HeapAlloc over 100,000 entries, the estimate lands within 6%
// and slightly high - 1.014, 1.061, 1.016 and 1.002 times the real figure at
// value lengths of 8, 64, 512 and 4096 bytes. Erring high is the safe
// direction for a bound: it evicts a little early rather than overshooting.
//
// It is still an estimate, so a memory bound is a target rather than a
// guarantee. Redis, which can ask its allocator directly, says much the same
// about maxmemory. There is a test comparing the two so this comment cannot
// quietly go out of date.
func (d *Dict) entryBytes(key string, obj *Obj) uint64 {
	n := uint64(entryOverhead) + uint64(len(key)) + valueBytes(obj.Value)
	if _, hasTTL := d.expiredDictStore[obj]; hasTTL {
		n += expiryOverhead
	}
	return n
}

func valueBytes(v interface{}) uint64 {
	switch t := v.(type) {
	case string:
		return uint64(len(t))
	case []byte:
		return uint64(len(t))
	default:
		// Everything the dictionary stores today is a string. Anything else is
		// counted at its overhead only, which is honest about not knowing.
		return 0
	}
}

// MemUsed reports the estimated bytes held by the dictionary.
func (d *Dict) MemUsed() uint64 { return d.memUsed }

// Evicted reports how many keys eviction has removed.
func (d *Dict) Evicted() uint64 { return d.evicted }

// EntryBytes exposes the per-key estimate, for MEMORY USAGE.
func (d *Dict) EntryBytes(key string) (uint64, bool) {
	obj, exists := d.dictStore[key]
	if !exists {
		return 0, false
	}
	return d.entryBytes(key, obj), true
}

// overLimit reports whether either configured bound is exceeded.
func (d *Dict) overLimit() bool {
	if len(d.dictStore) > config.KeyNumberLimit {
		return true
	}
	return config.MaxMemory > 0 && d.memUsed > config.MaxMemory
}

// enforceLimits evicts until the dictionary is back inside its bounds.
//
// Unlike a key-count bound, where one insert can only ever put the dictionary
// one key over, a single large value can exceed a memory bound by any amount,
// so this loops. It stops when the dictionary is empty: a value larger than the
// whole budget would otherwise evict everything and still not fit, and throwing
// away the entire keyspace to fail anyway helps nobody. The write is allowed to
// stand, over budget, which is what Redis does under an allkeys policy.
func (d *Dict) enforceLimits() {
	for d.overLimit() && len(d.dictStore) > 0 {
		before := len(d.dictStore)
		d.evict()
		if len(d.dictStore) == before {
			// Eviction could not remove anything. Stop rather than spin.
			return
		}
	}
}
