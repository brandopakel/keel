// Package config holds the server's settings: defaults here, overridden by the
// flags in cmd/keel before anything reads them.
package config

// Where the server listens, and how many connections it will hold.
var (
	// Host is the interface to bind. The event loop binds IPv4 only, so this
	// has to be an IPv4 address; 0.0.0.0 is every interface, which is the
	// Redis default and, like Redis, a reason to keep the server behind a
	// firewall.
	Host = "0.0.0.0"
	Port = 8081
	// MaxConnection is the listen backlog, and the most descriptors one turn
	// of the event loop can be handed at once.
	MaxConnection = 20000
)

// KeyNumberLimit bounds the keyspace by count: once it is reached, a write
// evicts before it lands. MaxMemory below bounds it by bytes.
var KeyNumberLimit = 5000000

// MaxMemory bounds the dictionary in bytes. Zero means unbounded, and the key
// count limit still applies either way.
//
// The figure is an estimate rather than a measurement - Go offers no way to ask
// the allocator what a value cost - so it is a target, not a guarantee. See
// entryBytes in data_structure/memory.go.
var MaxMemory uint64 = 0

// The eviction policies, chosen with -evict. EvictFirst takes whichever key a
// sample turns up first, which is to say a random one.
const (
	EvictFirst int = iota
	LRU
	LFU
)

// EvictStrategy is the policy in force.
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

// The append-only file. Off by default, as it is in Redis: it costs a write
// syscall per event-loop cycle and, under FsyncAlways, a disk flush before
// every reply.
var (
	AOFEnabled  = false
	AOFFileName = "./keel-master.aof"
	AOFFsync    = FsyncEverySec
)

// LegacyAOFFileName is what the default log was called while the server was
// called memkv.
//
// It is still looked for, because the alternative is the worst failure this
// file has: a server started after the rename finds no log at the new default,
// replays nothing, and comes up empty next to a perfectly good log it did not
// look at. Nothing errors and nothing warns - the keyspace is just gone. The
// old name is read if it is there and the new one is not; it is never written.
var LegacyAOFFileName = "./memkv-master.aof"

// Active expiry: how hard the server looks for keys whose TTL has passed
// rather than waiting for something to read them.
//
// The sampling is Redis's. Twenty keys with a TTL are examined; if more than a
// quarter had fallen due, the keyspace probably holds many more and another
// round is drawn. Rounds are capped so one pass cannot become a long stall on a
// keyspace that is mostly expired - what is left over is found on the next
// pass, a tenth of a second later.
//
// Zero samples turns it off, leaving expiry lazy as it was.
var (
	ActiveExpireSamples = 20
	ActiveExpirePercent = 25
	ActiveExpireRounds  = 16
)

// CronIntervalMs is how often the event loop is woken to do work that is due
// because of the clock rather than because a client asked.
//
// The loop blocks in epoll or kqueue with no timeout, so without a poke an idle
// server never runs anything time-based at all - which is precisely the server
// on which unread expired keys pile up. Redis calls its equivalent serverCron
// and runs it at 10Hz by default; this is the same rate for the same reason.
var CronIntervalMs = 100

// When the log is rewritten automatically.
//
// The percentage is measured against the size the log was after the last
// rewrite, which is roughly the size the data needs. Growth past that is
// history: commands superseded by later ones, and keys since deleted. 100 means
// rewrite once the log has doubled, which is Redis's default and the same
// reasoning - half the file being dead weight is worth one pass to be rid of.
//
// The minimum stops a small server rewriting constantly. A 64MB log takes a
// moment to replay and costs nothing to keep, so doubling from 1KB to 2KB is
// not worth a rewrite even though it is 100% growth. Zero percentage turns
// automatic rewriting off; BGREWRITEAOF still works.
var (
	AOFAutoRewritePercentage = 100
	AOFAutoRewriteMinSize    = int64(64 * 1024 * 1024)
)

// How often the log is flushed to disk.
//
//	FsyncAlways   before replying, so an acknowledged write is a durable one
//	FsyncEverySec at most once a second; a crash loses up to a second
//	FsyncNever    when the operating system feels like it
//
// EverySec is the default for the reason Redis chose it: Always turns every
// cycle into a disk round trip, and on a single-threaded server that is a stall
// every other connection shares.
const (
	FsyncAlways   = "always"
	FsyncEverySec = "everysec"
	FsyncNever    = "no"
)
