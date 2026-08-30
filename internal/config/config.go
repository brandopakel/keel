package config

var Host = "0.0.0.0"
var Port = 8081
var MaxConnection = 20000
var KeyNumberLimit = 5000000

// MaxMemory bounds the dictionary in bytes. Zero means unbounded, and the key
// count limit still applies either way.
//
// The figure is an estimate rather than a measurement - Go offers no way to ask
// the allocator what a value cost - so it is a target, not a guarantee. See
// entryBytes in data_structure/memory.go.
var MaxMemory uint64 = 0

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

// LCSMaxCells bounds len(key1)*len(key2) for the LCS command, which is the
// number of cell comparisons it performs. Zero removes the bound.
//
// The default is 134217728, which is where Redis stops: its LCS builds an
// (n+1)(m+1) table of uint32 and refuses once that allocation would exceed
// proto-max-bulk-len, 512MB by default. memkv does not build the table, so the
// same figure is reached for an entirely different reason - it is a time
// budget. Commands run on one thread, so an LCS does not merely take a while,
// it takes the server away from every other client for the duration. Measured
// at 410 million cells per second on darwin/arm64, the default is about 330ms
// of stall in the worst case, which is a lot; it is set here to match what
// Redis will answer rather than to be comfortable. Lower it if tail latency
// matters more than accepting every input Redis accepts.
var LCSMaxCells uint64 = 134217728

// IOThreads is how many threads read, parse and write sockets, counting the
// event loop's own thread as one of them. 1 keeps everything on the loop, which
// is the default and what Redis defaults to as well.
//
// Command execution is never threaded whatever this is set to. That is the
// whole design: one thread touching the stores is what lets them be plain maps
// with no locking, and it is not worth trading for throughput.
var IOThreads = 1

var AOFFileName = "./memkv-master.aof"
