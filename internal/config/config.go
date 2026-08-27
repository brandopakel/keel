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

// LFULogFactor controls how quickly the logarithmic access counter saturates.
// Higher means a slower rise, so the counter distinguishes larger access counts
// at the cost of resolution among small ones. Redis calls this lfu-log-factor
// and defaults to 10.
var LFULogFactor = 10

// LFUDecayPeriod is how many accesses across the whole keyspace pass before an
// idle key's counter drops by one. Redis measures this in minutes of wall
// clock; measuring it in accesses instead ties forgetting to how busy the cache
// is rather than to how long the process has been running, and keeps eviction
// behaviour reproducible.
//
// The default is measured rather than guessed, against the two workloads that
// pull in opposite directions - a scan that should not displace a working set,
// and a working set that moves and should be followed:
//
//	period    scan resistance    stale kept    current kept
//	  none          99.2%           72.0%          75.4%
//	100000          99.2%           57.6%          91.8%
//	 10000          99.2%            1.0%         100.0%
//	  1000          70.4%            1.4%         100.0%
//	   100           1.2%              -              -
//
// Too short and frequency never accumulates, leaving LFU no better than LRU;
// too long and the cache fills with keys that were popular once.
var LFUDecayPeriod = 10000
var AOFFileName = "./memkv-master.aof"
