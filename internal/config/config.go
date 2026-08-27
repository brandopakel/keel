package config

var Host = "0.0.0.0"
var Port = 8081
var MaxConnection = 20000
var KeyNumberLimit = 5000000

const (
	EvictFirst int = 0
	LRU            = 1
	LFU            = 2
)

var EvictStrategy = EvictFirst

// LRUSamples is how many random keys an approximate-LRU eviction looks at
// before choosing one. Redis calls this maxmemory-samples and defaults to 5:
// scanning every key to find the true least-recently-used one would make
// eviction O(n) and is the whole reason the policy is approximate.
var LRUSamples = 5
var AOFFileName = "./memkv-master.aof"
