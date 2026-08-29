package data_structure

// Approximate LRU.
//
// True LRU wants every key ordered by access time, which means a linked list
// threaded through the keyspace and pointer writes on every read: memory per
// key, and a lookup that mutates. The approximation samples a handful of keys
// and evicts the one read longest ago, which is what Redis does and why
// maxmemory-samples exists.
//
// Under LRU the access word is simply the logical clock reading at the last
// access, so Score is the identity and the whole policy is the two lines in
// Touch and Score in evictor.go. The sampling, the pool and the shared clock -
// everything that is actually hard - is common with LFU.
