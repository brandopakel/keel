package main

import (
	"flag"
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/brandopakel/keel/internal/config"
	"github.com/brandopakel/keel/internal/core"
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
	mode               string
	evictPolicy        string
	maxKeys            int
	lruSamples         int
	lfuLogFactor       int
	lfuDecayPeriod     int
	maxMemory          string
	lcsMaxCells        uint64
	ioThreads          int
	appendOnly         bool
	appendFilename     string
	appendFsync        string
	aofRewritePct      int
	aofRewriteMin      string
	expireSamples      int
	cronIntervalMs     int
	showVersion        bool
	passwordEnv        string
	replicaPasswordEnv string
)

// parseFlags reads the command line into config. It runs before anything
// starts, from main rather than from an init, so that nothing else in the
// process can observe a setting before the flag that changes it has been read.
func parseFlags() {
	flag.BoolVar(&config.ReplicationFeed, "replication-feed", false, "experimental: enable bounded canonical replication feed")
	flag.StringVar(&config.ReplicaOf, "replicaof", "", "experimental: read-only replica of host:port")
	flag.StringVar(&replicaPasswordEnv, "primary-password-env", "", "environment variable holding the primary AUTH password")
	flag.BoolVar(&config.ReplicaTLS, "primary-tls", false, "verify TLS when connecting to the primary proxy")
	flag.BoolVar(&showVersion, "version", false, "print the version and exit")
	flag.StringVar(&config.Host, "host", config.Host,
		"IPv4 address to listen on; 0.0.0.0 is every interface")
	flag.IntVar(&config.Port, "port", config.Port, "TCP port to listen on")
	flag.StringVar(&mode, "mode", "kqueue", "io mode: kqueue (default) | kqueue-nobuf | net | net-small | net-direct | net-chan")
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
	flag.BoolVar(&config.AOFAsyncAppend, "aof-async-append", false, "experimental: append on a worker with one-batch command backpressure")
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
	flag.IntVar(&config.MaxConnection, "maxclients", config.MaxConnection, "maximum connected clients")
	flag.StringVar(&passwordEnv, "requirepass-env", "", "environment variable containing the required AUTH password")
	flag.Parse()
	if passwordEnv != "" {
		config.RequirePass = os.Getenv(passwordEnv)
		if config.RequirePass == "" {
			log.Fatal("-requirepass-env names an empty or missing environment variable")
		}
	}

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
	if config.AOFAsyncAppend && !appendOnly {
		log.Fatal("-aof-async-append requires -appendonly")
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
	if n > math.MaxInt64/mult {
		return 0, fmt.Errorf("size overflows signed 64-bit byte count")
	}
	return n * mult, nil
}

func main() {
	parseFlags()
	if showVersion {
		fmt.Println(versionLine())
		return
	}
	if err := runServer(); err != nil {
		log.Println(err)
		os.Exit(1)
	}
}

func runServer() error {
	if config.ReplicationFeed || config.ReplicaOf != "" {
		if !config.AOFEnabled || config.RequirePass == "" || mode != "kqueue" {
			return fmt.Errorf("replication requires authenticated AOF in kqueue mode")
		}
		if config.ReplicaOf != "" {
			host, port, err := net.SplitHostPort(config.ReplicaOf)
			n, parseErr := strconv.Atoi(port)
			if err != nil || parseErr != nil || host == "" || n < 1 || n > 65535 {
				return fmt.Errorf("replicaof requires host:port with a valid port")
			}
			if config.ReplicationFeed || config.MaxMemory != 0 {
				return fmt.Errorf("replica requires no feed and no local eviction limits")
			}
			config.ReplicaPassword = os.Getenv(replicaPasswordEnv)
			if config.ReplicaPassword == "" {
				return fmt.Errorf("replica requires -primary-password-env naming a nonempty variable")
			}
		}
	}
	if config.MaxConnection < 1 || config.MaxConnection > 100000 || config.Port < 1 || config.Port > 65535 {
		return fmt.Errorf("invalid port or maxclients (1..100000)")
	}
	if config.RequirePass != "" && mode != "kqueue" && mode != "kqueue-nobuf" {
		return fmt.Errorf("authentication requires an event-loop mode")
	}
	if config.KeyNumberLimit < 1 || config.LRUSamples < 1 || config.LFULogFactor < 0 || config.LFUDecayPeriod < 0 {
		return fmt.Errorf("invalid eviction limits")
	}
	if config.AOFEnabled && mode != "kqueue" {
		return fmt.Errorf("-appendonly requires -mode kqueue; other modes are benchmarks")
	}
	var serve func(*sync.WaitGroup) error
	switch mode {
	case "kqueue":
		serve = server.RunAsyncTCPServer
	case "kqueue-nobuf":
		server.WriteUnbuffered = true
		serve = server.RunAsyncTCPServer
	case "net", "net-small", "net-direct", "net-chan":
		variants := map[string]server.NetVariant{"net": server.NetVariantMutex, "net-small": server.NetVariantSmallBuf, "net-direct": server.NetVariantDirect, "net-chan": server.NetVariantChannel}
		server.ActiveNetVariant = variants[mode]
		serve = server.RunNetTCPServer
	default:
		return fmt.Errorf("unknown or unsupported mode %q", mode)
	}
	fmt.Printf("starting keel %s ...\n", config.BuildVersion())
	if err := server.StartAOF(); err != nil {
		return fmt.Errorf("appendonly: %w", err)
	}

	if err := core.InitReplication(); err != nil {
		return err
	}

	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	var wg sync.WaitGroup
	wg.Add(1)
	done := make(chan error, 1)
	go func() { done <- serve(&wg) }()
	select {
	case err := <-done:
		closeErr := core.CloseAOF()
		if err != nil {
			return err
		}
		return closeErr
	case <-signals:
		server.Stop()
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case err := <-done:
		closeErr := core.CloseAOF()
		if err != nil {
			return err
		}
		return closeErr
	case <-signals:
		return fmt.Errorf("second termination signal")
	case <-timer.C:
		return fmt.Errorf("shutdown exceeded five seconds")
	}
}

// versionLine is what -version prints: the version, and the commit when the
// toolchain recorded one. A pseudo-version already names the commit, so the
// revision is only added when it says something the version does not.
func versionLine() string {
	version := config.BuildVersion()
	if rev := config.BuildRevision(); rev != "" && !strings.Contains(version, rev) {
		return fmt.Sprintf("keel %s (%s)", version, rev)
	}
	return "keel " + version
}
