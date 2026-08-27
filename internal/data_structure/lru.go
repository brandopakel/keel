package data_structure

// Approximate LRU eviction.
//
// True LRU wants every key ordered by access time, which means a linked list
// threaded through the keyspace and pointer writes on every read: memory per
// key, and a lookup that mutates. The approximation samples a handful of keys
// and evicts the one read longest ago, which is what Redis does and why
// maxmemory-samples exists.
//
// The sampling and pooling live in eviction.go; all that is specific to LRU is
// the score.

// evictApproxLRU removes one key, approximately the least recently used.
//
// The score is the logical clock reading at the last access, so the lowest
// score is the key read longest ago.
func (d *Dict) evictApproxLRU() {
	d.evictBySampling(func(obj *Obj) uint64 { return obj.lruClock() })
}
