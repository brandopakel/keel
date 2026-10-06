package main

import (
	"flag"
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"os/signal"
	"runtime"
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
	maxMemoryBytes     uint64
	eviction           core.EvictionPolicy
	lcsMaxCells        uint64
	ioThreads          int
	host               string
	port               int
	maxClients         int
	requirePass        string
	concurrentAppend   bool
	primaryPassword    string
	primaryTLS         bool
	appendOnly         bool
	appendFilename     string
	appendFsync        string
	asyncAppend        bool
	aofRewritePct      int
	aofRewriteMin      string
	aofRewriteMinBytes int64
	replicaOf          string
	replicationFeed    bool
	replicationProto   int
	expireSamples      int
	cronIntervalMs     int
	showVersion        bool
	passwordEnv        string
	replicaPasswordEnv string
	profileDir         string
	shutdownTimeout    = 5 * time.Second
)

// parseFlags reads the command line into the values engineOptions maps onto
// the engine and serverOptions onto the server. It runs before anything
// starts, from main rather than from an init, so that nothing else in the
// process can observe a setting before the flag that changes it has been read.
func parseFlags() {
	flag.DurationVar(&shutdownTimeout, "shutdown-timeout", 5*time.Second,
		"grace period after SIGTERM/SIGINT for server cleanup and persistence sync; a second signal exits early")
	flag.StringVar(&profileDir, "profile-dir", "", "diagnostic only: create a fresh private directory for CPU/heap/allocation profiles on shutdown")
	flag.BoolVar(&replicationFeed, "replication-feed", false, "experimental: enable bounded canonical replication feed")
	flag.IntVar(&replicationProto, "replication-protocol", 1, "experimental replication protocol: 1 (alpha images) or 2 (streaming snapshots, operation deltas and recovery checkpoints)")
	flag.StringVar(&replicaOf, "replicaof", "", "experimental: read-only replica of host:port")
	flag.StringVar(&replicaPasswordEnv, "primary-password-env", "", "environment variable holding the primary AUTH password")
	flag.BoolVar(&primaryTLS, "primary-tls", false, "verify TLS when connecting to the primary proxy")
	flag.BoolVar(&showVersion, "version", false, "print the version and exit")
	flag.StringVar(&host, "host", "127.0.0.1",
		"IPv4 address to listen on; 0.0.0.0 is every interface")
	flag.IntVar(&port, "port", 8081, "TCP port to listen on")
	flag.StringVar(&mode, "mode", "kqueue", "io mode: kqueue (default) | kqueue-nobuf | net | net-small | net-direct | net-chan")
	flag.StringVar(&maxMemory, "maxmemory", "0",
		"bound the keyspace in bytes, e.g. 512mb or 2gb; 0 is unbounded")
	flag.IntVar(&maxKeys, "maxkeys", 0,
		"evict once the keyspace reaches this many keys; 0 is unbounded, as in Redis")
	flag.StringVar(&evictPolicy, "evict", "lru",
		"eviction policy when -maxkeys or -maxmemory is reached: lru | lfu | random")
	flag.IntVar(&lruSamples, "lru-samples", 5,
		"keys sampled per eviction; more is more accurate and slower")
	flag.IntVar(&lfuLogFactor, "lfu-log-factor", 10,
		"how slowly the LFU access counter rises; higher spans more accesses in 8 bits")
	flag.IntVar(&lfuDecayPeriod, "lfu-decay-period", 10000,
		"accesses before an idle LFU counter drops by one; 0 disables forgetting")
	flag.Uint64Var(&lcsMaxCells, "lcs-max-cells", 134217728,
		"largest len(key1)*len(key2) LCS will attempt; 0 is unbounded")
	flag.BoolVar(&asyncAppend, "aof-async-append", false, "experimental: append on a worker with one-batch command backpressure")
	flag.BoolVar(&concurrentAppend, "aof-concurrent-append", false, "experimental: overlap bounded string commands with worker appends; requires -aof-async-append")
	flag.BoolVar(&appendOnly, "appendonly", false,
		"log every write to an append-only file and replay it at startup")
	flag.StringVar(&appendFilename, "appendfilename", "./keel-master.aof",
		"where that log lives")
	flag.StringVar(&appendFsync, "appendfsync", string(core.FsyncEverySec),
		"how often the log reaches disk: always | everysec | no")
	flag.IntVar(&aofRewritePct, "auto-aof-rewrite-percentage", 100,
		"rewrite the log once it has grown this much past its size after the last "+
			"rewrite; 0 disables automatic rewriting")
	flag.StringVar(&aofRewriteMin, "auto-aof-rewrite-min-size", "64mb",
		"never rewrite automatically below this size")
	flag.IntVar(&expireSamples, "active-expire-samples", 20,
		"keys with a TTL sampled per expiry cycle; 0 leaves expiry lazy")
	flag.IntVar(&cronIntervalMs, "cron-interval-ms", 100,
		"how often the loop is woken for work that is due by the clock")
	flag.IntVar(&ioThreads, "io-threads", 1,
		"threads that read, parse and write sockets, including the event loop's own; "+
			"command execution stays on one thread whatever this is")
	flag.IntVar(&maxClients, "maxclients", 20000, "maximum connected clients")
	flag.StringVar(&passwordEnv, "requirepass-env", "", "environment variable containing the required AUTH password")
	flag.Parse()
	if shutdownTimeout <= 0 {
		log.Fatal("-shutdown-timeout must be positive")
	}
	requirePass = ""
	if passwordEnv != "" {
		requirePass = os.Getenv(passwordEnv)
		if requirePass == "" {
			log.Fatal("-requirepass-env names an empty or missing environment variable")
		}
	}

	parsed, err := parseSize(maxMemory)
	if err != nil {
		log.Fatalf("bad -maxmemory %q: %v", maxMemory, err)
	}
	maxMemoryBytes = parsed
	if ioThreads < 1 {
		log.Fatalf("-io-threads must be at least 1, got %d", ioThreads)
	}

	switch core.FsyncPolicy(appendFsync) {
	case core.FsyncAlways, core.FsyncEverySec, core.FsyncNever:
	default:
		log.Fatalf("unknown -appendfsync %q (want always, everysec or no)", appendFsync)
	}
	if concurrentAppend && !asyncAppend {
		log.Fatal("-aof-concurrent-append requires -aof-async-append")
	}
	if asyncAppend && !appendOnly {
		log.Fatal("-aof-async-append requires -appendonly")
	}

	rewriteMin, err := parseSize(aofRewriteMin)
	if err != nil {
		log.Fatalf("bad -auto-aof-rewrite-min-size %q: %v", aofRewriteMin, err)
	}
	if aofRewritePct < 0 {
		log.Fatalf("-auto-aof-rewrite-percentage must not be negative, got %d", aofRewritePct)
	}
	aofRewriteMinBytes = int64(rewriteMin)

	if expireSamples < 0 {
		log.Fatalf("-active-expire-samples must not be negative, got %d", expireSamples)
	}
	if cronIntervalMs < 1 {
		log.Fatalf("-cron-interval-ms must be at least 1, got %d", cronIntervalMs)
	}
	switch evictPolicy {
	case "lru":
		eviction = core.EvictLRU
	case "lfu":
		eviction = core.EvictLFU
	case "random":
		eviction = core.EvictRandom
	default:
		log.Fatalf("unknown -evict %q (want lru, lfu or random)", evictPolicy)
	}
}

// unboundedLogWarning is what a server logging its writes says at startup when
// nothing bounds its keyspace. The server has no key bound unless -maxkeys or
// -maxmemory sets one, as in Redis (docs/embedding-plan.md, "Decisions"); but
// unlike Redis's, a log rewrite here refuses a keyspace over
// core.RewriteKeyCeiling keys, so past that the log cannot be compacted.
func unboundedLogWarning() string {
	return fmt.Sprintf("warning: -appendonly with neither -maxkeys nor -maxmemory set: the keyspace is unbounded, "+
		"and a log rewrite refuses more than %d keys, so past that the log grows without being compacted",
		core.RewriteKeyCeiling)
}

// engineOptions are the engine settings the flags describe. A flag's zero
// that turns its setting off is passed as core.Off, because an option's zero
// is its default.
func engineOptions() core.Options {
	return core.Options{
		MaxMemory:       maxMemoryBytes,
		MaxKeys:         maxKeys,
		Eviction:        eviction,
		EvictionSamples: lruSamples,
		LFULogFactor:    offIfZero(lfuLogFactor),
		LFUDecayPeriod:  offIfZero(lfuDecayPeriod),
		LCSMaxCells:     lcsMaxCellsOption(lcsMaxCells),

		ActiveExpireSamples: offIfZero(expireSamples),

		AppendOnly:            appendOnly,
		AppendFilename:        appendFilename,
		Fsync:                 core.FsyncPolicy(appendFsync),
		AsyncAppend:           asyncAppend,
		AutoRewritePercentage: offIfZero(aofRewritePct),
		AutoRewriteMinSize:    offIfZero(aofRewriteMinBytes),

		ReplicaOf:           replicaOf,
		ReplicationFeed:     replicationFeed,
		ReplicationProtocol: replicationProto,
	}
}

// serverOptions are the server settings the flags describe, every one passed
// explicitly, as engineOptions passes the engine's.
func serverOptions() server.Options {
	return server.Options{
		Host:             host,
		Port:             port,
		MaxClients:       maxClients,
		IOThreads:        ioThreads,
		CronInterval:     time.Duration(cronIntervalMs) * time.Millisecond,
		RequirePass:      requirePass,
		ConcurrentAppend: concurrentAppend,
		PrimaryPassword:  primaryPassword,
		PrimaryTLS:       primaryTLS,
	}
}

// offIfZero maps a flag whose zero turns its setting off onto the option
// that does.
func offIfZero[T int | int64](n T) T {
	if n == 0 {
		return core.Off
	}
	return n
}

// lcsMaxCellsOption maps -lcs-max-cells, where zero is no bound, onto the
// option. No product of two lengths reaches 2^63, so a bound past it is no
// bound either.
func lcsMaxCellsOption(n uint64) int64 {
	if n == 0 || n > math.MaxInt64 {
		return core.Off
	}
	return int64(n)
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
	finishProfiles, err := startProfiles(profileDir)
	if err != nil {
		log.Println(err)
		os.Exit(1)
	}
	err = runServer()
	profileErr := finishProfiles()
	if err != nil {
		log.Println(err)
		os.Exit(1)
	}
	if profileErr != nil {
		log.Println(profileErr)
		os.Exit(1)
	}
}

func runServer() error {
	if replicationProto != 1 && replicationProto != 2 {
		return fmt.Errorf("-replication-protocol must be 1 or 2")
	}
	if replicationFeed || replicaOf != "" {
		if !appendOnly || requirePass == "" || mode != "kqueue" {
			return fmt.Errorf("replication requires authenticated AOF in kqueue mode")
		}
		if replicaOf != "" {
			host, port, err := net.SplitHostPort(replicaOf)
			n, parseErr := strconv.Atoi(port)
			if err != nil || parseErr != nil || host == "" || n < 1 || n > 65535 {
				return fmt.Errorf("replicaof requires host:port with a valid port")
			}
			if replicationFeed || maxMemoryBytes != 0 {
				return fmt.Errorf("replica requires no feed and no local eviction limits")
			}
			primaryPassword = os.Getenv(replicaPasswordEnv)
			if primaryPassword == "" {
				return fmt.Errorf("replica requires -primary-password-env naming a nonempty variable")
			}
		}
	}
	if maxClients < 1 || maxClients > 100000 || port < 1 || port > 65535 {
		return fmt.Errorf("invalid port or maxclients (1..100000)")
	}
	if requirePass != "" && mode != "kqueue" && mode != "kqueue-nobuf" {
		return fmt.Errorf("authentication requires an event-loop mode")
	}
	if maxKeys < 0 || lruSamples < 1 || lfuLogFactor < 0 || lfuDecayPeriod < 0 {
		return fmt.Errorf("invalid eviction limits")
	}
	if appendOnly && mode != "kqueue" {
		return fmt.Errorf("-appendonly requires -mode kqueue; other modes are benchmarks")
	}
	var serve func(*sync.WaitGroup, server.Options) error
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
	// The engine's settings are the flags', every one passed explicitly, so
	// that none of the server's is an engine default. They are in place before
	// the log is replayed, as the variables they replace were assigned before
	// anything read them.
	if err := core.Configure(engineOptions()); err != nil {
		return err
	}
	if appendOnly && maxKeys == 0 && maxMemoryBytes == 0 {
		log.Println(unboundedLogWarning())
	}
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
	go func() {
		err := serve(&wg, serverOptions())
		closeErr := core.CloseAOF()
		if err != nil {
			done <- err
		} else {
			done <- closeErr
		}
	}()
	select {
	case err := <-done:
		return err
	case <-signals:
		server.Stop()
	}
	return waitForShutdown(done, signals, shutdownTimeout)
}

func waitForShutdown(done <-chan error, signals <-chan os.Signal, grace time.Duration) error {
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-signals:
		return fmt.Errorf("second termination signal")
	case <-timer.C:
		// Capture the blocked shutdown phase before main exits. A timeout
		// alone cannot distinguish a parked event loop from slow persistence.
		// Bound the diagnostic allocation/output even with many connections.
		stack := make([]byte, 1<<20)
		n := runtime.Stack(stack, true)
		log.Printf("shutdown timeout goroutine dump (truncated=%t):\n%s", n == len(stack), stack[:n])
		return fmt.Errorf("shutdown exceeded %s", grace)
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
