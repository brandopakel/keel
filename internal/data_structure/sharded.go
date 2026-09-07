package data_structure

import "hash/maphash"

// A keyspace is stored as a fixed number of shards rather than one Go map, so
// that walking it can stop and resume at a shard boundary.
//
// Redis can resume a walk because its own hash table exposes buckets, and its
// SCAN cursor is a bucket index visited in reverse-binary order. A Go map
// exposes nothing of the sort and randomises its iteration order on every
// range, so an offset into one is meaningless the moment the range ends. The
// choice is therefore between copying every key name to index into, keeping a
// sorted index updated on every write, and partitioning the keys so that a
// bounded part of them can be taken whole.
//
// This is the third. The shard a key belongs to is a hash of its name, so it
// never changes while the key exists, which is what makes the partition a
// usable cursor: each shard is emitted exactly once, so a key present for the
// whole walk is reported exactly once - no duplicates, which is stronger than
// what Redis's SCAN promises. Keys created or removed mid-walk may or may not
// be seen, which is the same as Redis.
//
// The cursor is the next shard index, so it is a small integer the client hands
// back verbatim and the server holds no per-cursor state at all. Nothing has to
// expire an abandoned cursor, and a client that walks away costs nothing.
//
// The count balances two costs that pull opposite ways, and was chosen by
// measuring both rather than by picking a round number.
//
// Too many shards and the fixed cost of a map holding almost nothing is paid
// over and over. That is not hypothetical, and it is why this constant is sized
// against the floor go.mod declares rather than against the newest toolchain:
// at 1024 shards and 100,000 keys it cost 21.8 bytes per key under Go 1.22,
// enough that the keyspace estimate stopped bounding the real heap and
// TestEstimateTracksRealHeap failed. Go 1.24 replaced the map implementation
// and the cost went away, so the number a measurement gives depends on which
// toolchain took it.
//
// Too few and a shard is a large piece to take whole, because a call emits one
// and cannot stop inside it. Five million keys, which is what KeyNumberLimit
// allows, come to about 4,900 per shard here - the largest reply and the
// longest pause a single call can produce.
//
// 1024 is therefore what the floor allows rather than what it forces: while
// go.mod claimed Go 1.22 this had to be 256, and raising the floor to a release
// that still gets security fixes is what bought back the finer granularity.
const shardCount = 1024

// shardSeed is per process. Shard membership only has to be stable for as long
// as a cursor is live, which is within one process: a cursor does not survive a
// restart in Redis either. A fresh seed each start also means key names cannot
// be chosen ahead of time to pile into one shard.
var shardSeed = maphash.MakeSeed()

func shardOf(key string) int {
	return int(maphash.String(shardSeed, key) & (shardCount - 1))
}

// shardedMap is a map[string]V split across shards, with the operations the
// keyspaces need. Shards are allocated on first write, so an empty keyspace
// costs one nil pointer per shard rather than an empty map per shard.
type shardedMap[V any] struct {
	shards [shardCount]map[string]V
	count  int
}

func (m *shardedMap[V]) get(key string) (V, bool) {
	v, ok := m.shards[shardOf(key)][key]
	return v, ok
}

// set stores v at key. It reports whether the key was already present, which is
// what the callers need in order to account an overwrite as a replacement
// rather than as growth.
func (m *shardedMap[V]) set(key string, v V) (existed bool) {
	i := shardOf(key)
	shard := m.shards[i]
	if shard == nil {
		shard = make(map[string]V)
		m.shards[i] = shard
	}
	_, existed = shard[key]
	shard[key] = v
	if !existed {
		m.count++
	}
	return existed
}

func (m *shardedMap[V]) del(key string) bool {
	i := shardOf(key)
	shard := m.shards[i]
	if _, ok := shard[key]; !ok {
		return false
	}
	delete(shard, key)
	m.count--
	// An emptied shard gives its map back rather than holding the buckets a
	// burst of keys grew. Re-creating one is a single allocation on the next
	// write to it.
	if len(shard) == 0 {
		m.shards[i] = nil
	}
	return true
}

// len is maintained rather than summed, because a budget check consults it on
// every write and summing would make that proportional to the shard count.
func (m *shardedMap[V]) len() int { return m.count }

func (m *shardedMap[V]) keys() []string {
	keys := make([]string, 0, m.count)
	for _, shard := range m.shards {
		for key := range shard {
			keys = append(keys, key)
		}
	}
	return keys
}

// scan walks whole shards from cursor, appending the keys keep accepts to dst,
// and returns how many it examined and the cursor to pass back - zero once the
// walk is complete.
//
// budget bounds keys examined rather than keys returned, which is what makes
// the work per call bounded whatever the filter rejects. A selective filter
// therefore yields short or empty batches with a cursor still to follow, the
// same way a Redis SCAN with MATCH does.
//
// Shards are taken whole because a shard is the unit that can be resumed, so a
// call overruns its budget by at most the size of one shard. An empty shard
// costs a nil check, so a small keyspace finishes in one call rather than
// making the client ask a thousand times for nothing.
func (m *shardedMap[V]) scan(cursor uint64, budget int, keep func(string) bool, dst []string) ([]string, int, uint64) {
	if cursor >= shardCount {
		// Not an error. A cursor from another keyspace, or from before a
		// restart, has simply run past the end of this one.
		return dst, 0, 0
	}
	if budget < 1 {
		budget = 1
	}
	examined := 0
	for i := int(cursor); i < shardCount; i++ {
		// keep may reap the key it is shown, and deleting from a map while
		// ranging over it is defined; the range holds this shard's map even if
		// emptying it clears the slot.
		for key := range m.shards[i] {
			examined++
			if keep == nil || keep(key) {
				dst = append(dst, key)
			}
		}
		if examined >= budget {
			// i+1 is the next shard, and equals shardCount when this was the
			// last one, which the caller reads as complete.
			if i+1 >= shardCount {
				return dst, examined, 0
			}
			return dst, examined, uint64(i + 1)
		}
	}
	return dst, examined, 0
}

// sampleState advances a shard start between calls. A generator of its own
// keeps eviction sampling off the lock inside math/rand, which is the same
// reason skiplist.go carries one.
var sampleState = uint64(0x9e3779b97f4a7c15)

func nextSampleStart() int {
	sampleState ^= sampleState << 13
	sampleState ^= sampleState >> 7
	sampleState ^= sampleState << 17
	return int(sampleState & (shardCount - 1))
}

// sample visits up to n entries, starting from a shard chosen by that generator
// and walking forward over shards until it has enough.
//
// Starting at a moving shard rather than choosing shards at random is what
// makes this work when the keyspace is small: a few keys spread over a thousand
// shards would leave independent shard draws finding nothing almost every time,
// and eviction would then have no candidates to weigh while the budget was
// already exceeded.
func (m *shardedMap[V]) sample(n int, visit func(key string, value V)) {
	if m.count == 0 || n < 1 {
		return
	}
	taken := 0
	start := nextSampleStart()
	for offset := 0; offset < shardCount; offset++ {
		for key, value := range m.shards[(start+offset)&(shardCount-1)] {
			visit(key, value)
			if taken++; taken >= n {
				return
			}
		}
	}
}

// ScanKeyspaces walks every registered keyspace under a single cursor.
//
// A key name belongs to exactly one keyspace, so a client scanning the server
// has to be walked across all of them without having to know they exist. The
// cursor therefore carries which store it is in as well as where in it, packed
// as index*shardCount + shard. That stays a small integer, and a cursor from a
// keyspace layout that no longer matches simply runs off the end and reports
// the walk as finished rather than reading the wrong store.
//
// keep is given the keyspace as well as the key, so a caller can reject a whole
// store by name, or ask that store whether the key is still live, without
// having to search every store for the one that owns the name.
func ScanKeyspaces(cursor uint64, budget int, keep func(Keyspace, string) bool, dst []string) ([]string, uint64) {
	if budget < 1 {
		budget = 1
	}
	index, shard := int(cursor/shardCount), cursor%shardCount
	examined := 0
	for ; index < len(keyspaces); index++ {
		ks := keyspaces[index]
		var (
			n    int
			next uint64
		)
		filter := func(key string) bool { return keep == nil || keep(ks, key) }
		dst, n, next = ks.Scan(shard, budget-examined, filter, dst)
		examined += n
		if next != 0 {
			return dst, uint64(index)*shardCount + next
		}
		// This store is exhausted. Resume in the next one, at its first shard.
		shard = 0
		if examined >= budget {
			if index+1 >= len(keyspaces) {
				return dst, 0
			}
			return dst, uint64(index+1) * shardCount
		}
	}
	return dst, 0
}
