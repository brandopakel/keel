package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/brandopakel/keel/internal/config"
	"github.com/brandopakel/keel/internal/server"
)

// mode selects the I/O implementation, so the alternatives discussed in the
// upstream performance issue can be benchmarked against each other from one
// binary:
//
//	kqueue        event loop, replies coalesced per read       (default)
//	kqueue-nobuf  event loop, one write syscall per reply       (upstream's design)
//	net           net.Listener with one goroutine per connection
//
// The default coalesces. One write per command is a throughput ceiling rather
// than a trade-off, so serving unbuffered has to be asked for explicitly with
// -mode kqueue-nobuf.
var (
	mode           string
	evictPolicy    string
	maxKeys        int
	lruSamples     int
	lfuLogFactor   int
	lfuDecayPeriod int
	maxMemory      string
	lcsMaxCells    uint64
	ioThreads      int
	appendOnly     bool
	appendFilename string
	appendFsync    string
	aofRewritePct  int
	aofRewriteMin  string
	expireSamples  int
	cronIntervalMs int
)

func init() {
	flag.StringVar(&config.Host, "host", "0.0.0.0", "host")
	flag.IntVar(&config.Port, "port", config.Port, "port")
	flag.StringVar(&mode, "mode", "kqueue", "io mode: kqueue (default) | kqueue-nobuf | net | net-small | net-direct | net-chan | net-nolock")
	flag.StringVar(&maxMemory, "maxmemory", "0",
		"bound the keyspace in bytes, e.g. 512mb or 2gb; 0 is unbounded")
	flag.IntVar(&maxKeys, "maxkeys", config.KeyNumberLimit,
		"evict once the keyspace reaches this many keys")
	flag.StringVar(&evictPolicy, "evict", "lru",
		"eviction policy when -maxkeys is reached: lru | lfu | random")
	flag.IntVar(&lruSamples, "lru-samples", config.LRUSamples,
		"keys sampled per eviction; more is more accurate and slower")
	flag.IntVar(&lfuLogFactor, "lfu-log-factor", config.LFULogFactor,
		"how slowly the LFU access counter rises; higher spans more accesses in 8 bits")
	flag.IntVar(&lfuDecayPeriod, "lfu-decay-period", config.LFUDecayPeriod,
		"accesses before an idle LFU counter drops by one; 0 disables forgetting")
	flag.Uint64Var(&lcsMaxCells, "lcs-max-cells", config.LCSMaxCells,
		"largest len(key1)*len(key2) LCS will attempt; 0 is unbounded")
	flag.BoolVar(&appendOnly, "appendonly", config.AOFEnabled,
		"log every write to an append-only file and replay it at startup")
	flag.StringVar(&appendFilename, "appendfilename", config.AOFFileName,
		"where that log lives")
	flag.StringVar(&appendFsync, "appendfsync", config.AOFFsync,
		"how often the log reaches disk: always | everysec | no")
	flag.IntVar(&aofRewritePct, "auto-aof-rewrite-percentage", config.AOFAutoRewritePercentage,
		"rewrite the log once it has grown this much past its size after the last "+
			"rewrite; 0 disables automatic rewriting")
	flag.StringVar(&aofRewriteMin, "auto-aof-rewrite-min-size", "64mb",
		"never rewrite automatically below this size")
	flag.IntVar(&expireSamples, "active-expire-samples", config.ActiveExpireSamples,
		"keys with a TTL sampled per expiry cycle; 0 leaves expiry lazy")
	flag.IntVar(&cronIntervalMs, "cron-interval-ms", config.CronIntervalMs,
		"how often the loop is woken for work that is due by the clock")
	flag.IntVar(&ioThreads, "io-threads", config.IOThreads,
		"threads that read, parse and write sockets, including the event loop's own; "+
			"command execution stays on one thread whatever this is")
	flag.Parse()

	parsed, err := parseSize(maxMemory)
	if err != nil {
		log.Fatalf("bad -maxmemory %q: %v", maxMemory, err)
	}
	config.MaxMemory = parsed
	config.KeyNumberLimit = maxKeys
	config.LRUSamples = lruSamples
	config.LFULogFactor = lfuLogFactor
	config.LFUDecayPeriod = lfuDecayPeriod
	config.LCSMaxCells = lcsMaxCells
	if ioThreads < 1 {
		log.Fatalf("-io-threads must be at least 1, got %d", ioThreads)
	}
	config.IOThreads = ioThreads

	switch appendFsync {
	case config.FsyncAlways, config.FsyncEverySec, config.FsyncNever:
	default:
		log.Fatalf("unknown -appendfsync %q (want always, everysec or no)", appendFsync)
	}
	config.AOFEnabled = appendOnly
	config.AOFFileName = appendFilename
	config.AOFFsync = appendFsync

	rewriteMin, err := parseSize(aofRewriteMin)
	if err != nil {
		log.Fatalf("bad -auto-aof-rewrite-min-size %q: %v", aofRewriteMin, err)
	}
	if aofRewritePct < 0 {
		log.Fatalf("-auto-aof-rewrite-percentage must not be negative, got %d", aofRewritePct)
	}
	config.AOFAutoRewritePercentage = aofRewritePct
	config.AOFAutoRewriteMinSize = int64(rewriteMin)

	if expireSamples < 0 {
		log.Fatalf("-active-expire-samples must not be negative, got %d", expireSamples)
	}
	if cronIntervalMs < 1 {
		log.Fatalf("-cron-interval-ms must be at least 1, got %d", cronIntervalMs)
	}
	config.ActiveExpireSamples = expireSamples
	config.CronIntervalMs = cronIntervalMs
	switch evictPolicy {
	case "lru":
		config.EvictStrategy = config.LRU
	case "lfu":
		config.EvictStrategy = config.LFU
	case "random":
		config.EvictStrategy = config.EvictFirst
	default:
		log.Fatalf("unknown -evict %q (want lru, lfu or random)", evictPolicy)
	}
}

// parseSize reads a byte count, accepting the k/m/g suffixes people actually
// type rather than demanding a raw number of bytes.
func parseSize(s string) (uint64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	mult := uint64(1)
	for suffix, m := range map[string]uint64{"kb": 1 << 10, "mb": 1 << 20, "gb": 1 << 30} {
		if strings.HasSuffix(s, suffix) {
			s, mult = strings.TrimSuffix(s, suffix), m
			break
		}
	}
	n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, err
	}
	return n * mult, nil
}

func main() {
	fmt.Println("starting keel ...")
	var signals = make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	var wg sync.WaitGroup
	wg.Add(2)

	// Loaded before any loop starts. Doing it inside the server goroutine would
	// race the listener: a client could connect and be answered out of a
	// keyspace that is still being replayed into.
	if err := server.StartAOF(); err != nil {
		log.Fatalf("appendonly: %v", err)
	}

	switch mode {
	case "kqueue":
		go server.RunAsyncTCPServer(&wg)
	case "kqueue-nobuf":
		server.WriteUnbuffered = true
		go server.RunAsyncTCPServer(&wg)
	case "net":
		server.ActiveNetVariant = server.NetVariantMutex
		go server.RunNetTCPServer(&wg)
	case "net-small":
		server.ActiveNetVariant = server.NetVariantSmallBuf
		go server.RunNetTCPServer(&wg)
	case "net-direct":
		server.ActiveNetVariant = server.NetVariantDirect
		go server.RunNetTCPServer(&wg)
	case "net-chan":
		server.ActiveNetVariant = server.NetVariantChannel
		go server.RunNetTCPServer(&wg)
	case "net-nolock":
		// diagnostic only: PING-safe, races on any command that touches a store
		server.EvalUnlocked = true
		go server.RunNetTCPServer(&wg)
	case "kqueue-wbuf":
		// Retired 2026-08-27. Kept as a named case so a stale command line says
		// what happened instead of just failing to match.
		log.Fatalf("-mode kqueue-wbuf was renamed: coalescing is now the default -mode kqueue, " +
			"and the old unbuffered -mode kqueue is now -mode kqueue-nobuf")
	default:
		log.Fatalf("unknown -mode %q (want kqueue, kqueue-nobuf or net*)", mode)
	}
	go server.WaitForSignal(&wg, signals)

	wg.Wait()
}
