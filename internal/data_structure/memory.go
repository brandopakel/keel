package data_structure

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
//
// The constant holds from Go 1.22 onwards, which is why go.mod requires it. On
// Go 1.21 the same workload holds substantially more heap, and since the
// overhead is per entry the error is worst where the value is smallest.
// Measured on linux/amd64 by TestEstimateTracksRealHeap, as estimate/actual:
//
//	Go        val=8    val=64   val=512   val=4096
//	1.21.13   0.611     0.723     0.904      0.984
//	1.22.12   0.977     1.031     1.008      1.001
//	1.23.12   0.978     1.032     1.009      1.001
//	1.24.9    1.017     1.061     1.016      1.002
//	1.26.6    1.017     1.061     1.016      1.002
//
// Under-counting by 39% is not a cosmetic drift: -maxmemory is enforced against
// this number, so a budget set on Go 1.21 admits far more than it was asked to
// and the bound stops bounding. Requiring 1.22 is the fix rather than widening
// the test, because the accounting either describes the heap or it does not.
const entryOverhead = 100

// expiryOverhead is the additional cost of a key with a TTL, which lives in a
// second map keyed by object pointer.
const expiryOverhead = 48

// Per-member and per-structure costs for the collection types, measured the
// same way - fill one with 200,000 members and read HeapAlloc either side:
//
//	map[string]struct{}    59 B per 20-byte member  ->  39 of overhead
//	map[string]float64     59 B per 20-byte member  ->  39 of overhead
//	ZSet (dict+skiplist)  155 B per 20-byte member  -> 135 of overhead
//
// A hash is map[string]string, which carries a second string header per entry.
// Measured the same way over 200,000 fields, as bytes per field minus the field
// and the value themselves:
//
//	           value=10   value=20   value=40
//	field=10       64.7       62.5       66.5
//	field=20       62.5       60.5       64.5
//	field=40       66.5       64.5       68.5
//
// 64 is the middle of that. The spread is the allocator rounding each field and
// value up to a size class, which is the part an estimate cannot follow, and
// the reason the test asserts a band rather than a number.
//
// A list is charged in two parts rather than one, because two different things
// scale differently. Its buffer is a []string, so every slot is exactly a
// 16-byte string header whether or not an element is in it - that is charged at
// capacity, since a list that grew and shrank still owns the slots. The
// allocator's rounding of the element strings themselves scales with the number
// of elements instead, and measures at about 5 bytes each over the mixed sizes
// below. Folding both into one per-slot constant fits a full list and
// over-counts a list that has shrunk by a quarter, which is the direction a
// memory bound should not be wrong in.
//
// The base figures are the empty structure: the struct itself, its map header
// and, for a sorted set, the skiplist head node with its 32 levels.
const (
	setMemberOverhead  = 39
	setBaseBytes       = 64
	hashFieldOverhead  = 64
	hashBaseBytes      = 64
	listSlotOverhead   = 16
	listElemOverhead   = 5
	listBaseBytes      = 64
	zsetMemberOverhead = 135
	zsetBaseBytes      = 640
	cmsBaseBytes       = 64
	morrisBaseBytes    = 64
	// A dense HyperLogLog's 12289-byte register array does not land on a size
	// class, so the allocator rounds it up to 13568. Measured, 200 sketches
	// cost 13616 bytes each, so the difference is charged here rather than
	// leaving the estimate 9% light.
	hllBaseBytes     = 1327
	sbChainBaseBytes = 64
	bloomBaseBytes   = 96
)

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
	if _, hasTTL := d.expiredDictStore[key]; hasTTL {
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

// EntryBytes exposes the per-key estimate, for MEMORY USAGE.
func (d *Dict) EntryBytes(key string) (uint64, bool) {
	obj, exists := d.dictStore[key]
	if !exists {
		return 0, false
	}
	return d.entryBytes(key, obj), true
}
