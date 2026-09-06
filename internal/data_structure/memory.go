package data_structure

// Memory accounting, so the keyspace can be bounded by bytes rather than by
// key count.
//
// A key limit treats an 8-byte value and an 8MB value as equally expensive,
// which is the wrong question for a cache: what runs out is memory. Bounding by
// bytes means knowing what a key costs, and Go gives no way to ask the
// allocator, so the cost is estimated.
//
// String entries use a typed 24-byte Obj. The former interface-based object
// needed 32 bytes plus a separately allocated 16-byte string header. Removing
// those 24 bytes changes the calibrated overhead from 100 to 76 bytes per key.
// This includes the map slot and allocator slack; TestEstimateTracksRealHeap
// checks the estimate against live heap growth on every supported Go version.
const stringEntryOverhead = 76

// Collection keyspaces still use their existing entry representation.
const entryOverhead = 100

// expiryOverhead is the additional cost of a key with a TTL, which lives in a
// second map keyed by key name.
const expiryOverhead = 48

// Per-member and per-structure costs for the collection types, measured the
// same way - fill one with 200,000 members and read HeapAlloc either side:
//
//	Set (map[string]int + []string)
//	                       78 B per 20-byte member  ->  58 of overhead;
//	                       60 and 62 at 10 and 40 bytes, so 60 is used
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
	setMemberOverhead  = 60
	setBaseBytes       = 96
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
	// plus its 64-byte object costs 13632 bytes; charge the difference rather than
	// leaving the estimate 9% light.
	hllBaseBytes     = 1343
	sbChainBaseBytes = 64
	bloomBaseBytes   = 96
)

// entryBytes estimates the retained string entry and optional expiry record.
// Allocator rounding and map occupancy vary; a maxmemory budget is an estimated
// keyspace target, not a process RSS bound.
func (d *Dict) entryBytes(key string, obj *Obj) uint64 {
	n := uint64(stringEntryOverhead) + uint64(len(key)) + uint64(len(obj.Value))
	if _, hasTTL := d.expiredDictStore[key]; hasTTL {
		n += expiryOverhead
	}
	return n
}

// EntryBytes exposes the per-key estimate, for MEMORY USAGE.
func (d *Dict) EntryBytes(key string) (uint64, bool) {
	obj, exists := d.dictStore[key]
	if !exists {
		return 0, false
	}
	return d.entryBytes(key, obj), true
}
