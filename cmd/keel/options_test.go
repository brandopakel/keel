package main

import (
	"flag"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/brandopakel/keel/internal/server"

	"github.com/brandopakel/keel/internal/core"
	"github.com/brandopakel/keel/internal/data_structure"
)

// configureFrom parses args as the server's command line and holds the
// default engine to the options they describe, as runServer does, then
// returns the limits that engine's space is held to. The flags and the engine
// are put back when t ends.
func configureFrom(t *testing.T, args ...string) data_structure.Limits {
	t.Helper()
	commandLine, argv, options := flag.CommandLine, os.Args, core.Configuration()
	t.Cleanup(func() {
		flag.CommandLine, os.Args = commandLine, argv
		if err := core.Configure(options); err != nil {
			t.Error(err)
		}
	})
	flag.CommandLine = flag.NewFlagSet("keel", flag.ContinueOnError)
	os.Args = append([]string{"keel"}, args...)
	parseFlags()
	if err := core.Configure(engineOptions()); err != nil {
		t.Fatal(err)
	}
	return data_structure.DefaultSpace.Limits()
}

// TestDefaultFlagsKeepTheServerSettings pins what the server's engine is held
// to with no flags, exactly as before the flags were mapped onto engine
// options rather than into config: the 5,000,000-key cap and LRU, no memory
// bound, and the sampling, LFU and LCS figures; active expiry at 20 keys a
// round, 25% and 16 rounds; no log, at ./keel-master.aof under everysec
// when there is one, appended on the caller's thread and rewritten at 100%
// growth past 64 MiB; and neither a replica nor a feed, in protocol 1.
func TestDefaultFlagsKeepTheServerSettings(t *testing.T) {
	got := configureFrom(t)
	want := data_structure.Limits{Eviction: data_structure.EvictLRU, MaxKeys: 5000000, MaxMemory: 0,
		EvictionSamples: 5, LFULogFactor: 10, LFUDecayPeriod: 10000, LCSMaxCells: 134217728}
	if got != want {
		t.Fatalf("default flags hold the engine to %+v, want %+v", got, want)
	}
	wantOptions := core.Options{MaxKeys: 5000000, Eviction: core.EvictLRU, EvictionSamples: 5,
		LFULogFactor: 10, LFUDecayPeriod: 10000, LCSMaxCells: 134217728,
		ActiveExpireSamples: 20, ActiveExpirePercent: 25, ActiveExpireRounds: 16,
		AppendFilename: "./keel-master.aof", Fsync: core.FsyncEverySec,
		AutoRewritePercentage: 100, AutoRewriteMinSize: 64 << 20, ReplicationProtocol: 1}
	if options := core.Configuration().WithDefaults(); options != wantOptions {
		t.Fatalf("default flags give the engine %+v, want %+v", options, wantOptions)
	}
}

// TestFlagsReachTheEngine: every keyspace flag reaches the engine as given,
// and a flag's zero that turns its setting off turns it off, rather than
// becoming the option's default.
func TestFlagsReachTheEngine(t *testing.T) {
	got := configureFrom(t, "-maxmemory", "2mb", "-maxkeys", "7", "-evict", "lfu", "-lru-samples", "9",
		"-lfu-log-factor", "3", "-lfu-decay-period", "40", "-lcs-max-cells", "1000")
	want := data_structure.Limits{Eviction: data_structure.EvictLFU, MaxKeys: 7, MaxMemory: 2 << 20,
		EvictionSamples: 9, LFULogFactor: 3, LFUDecayPeriod: 40, LCSMaxCells: 1000}
	if got != want {
		t.Fatalf("flags hold the engine to %+v, want %+v", got, want)
	}

	got = configureFrom(t, "-evict", "random", "-lfu-log-factor", "0", "-lfu-decay-period", "0", "-lcs-max-cells", "0")
	want = data_structure.Limits{Eviction: data_structure.EvictRandom, MaxKeys: 5000000,
		EvictionSamples: 5, LFULogFactor: 0, LFUDecayPeriod: 0, LCSMaxCells: 0}
	if got != want {
		t.Fatalf("zero flags hold the engine to %+v, want %+v", got, want)
	}

	got = configureFrom(t, "-lcs-max-cells", "18446744073709551615")
	if got.LCSMaxCells != 0 {
		t.Fatalf("an LCS bound past 2^63 holds the engine to %d cells, want no bound", got.LCSMaxCells)
	}

	configureFrom(t, "-active-expire-samples", "7", "-appendonly", "-appendfilename", "/tmp/x.aof",
		"-appendfsync", "always", "-aof-async-append", "-auto-aof-rewrite-percentage", "50",
		"-auto-aof-rewrite-min-size", "1mb", "-replication-feed", "-replication-protocol", "2")
	options := core.Configuration()
	if options.ActiveExpireSamples != 7 || !options.AppendOnly || options.AppendFilename != "/tmp/x.aof" ||
		options.Fsync != core.FsyncAlways || !options.AsyncAppend || options.AutoRewritePercentage != 50 ||
		options.AutoRewriteMinSize != 1<<20 || !options.ReplicationFeed || options.ReplicationProtocol != 2 {
		t.Fatalf("flags give the engine %+v", options)
	}
	configureFrom(t, "-replicaof", "primary.test:6379")
	if options := core.Configuration(); options.ReplicaOf != "primary.test:6379" {
		t.Fatalf("-replicaof gives the engine %+v", options)
	}

	configureFrom(t, "-active-expire-samples", "0", "-auto-aof-rewrite-percentage", "0",
		"-auto-aof-rewrite-min-size", "0")
	options = core.Configuration()
	if options.ActiveExpireSamples >= 0 || options.AutoRewritePercentage >= 0 || options.AutoRewriteMinSize >= 0 {
		t.Fatalf("zero flags give the engine %+v, want each of them off", options)
	}
}

// TestServerKeyCapCoversTheRewriteCeiling: a rewrite ceiling below the
// keyspace the server holds by default is worse than none - auto-rewrite
// retries every minute, fails every time, and the log grows without bound -
// and one above it is dead configuration. This keeps the two numbers in a
// sane relation to each other.
func TestServerKeyCapCoversTheRewriteCeiling(t *testing.T) {
	if core.RewriteKeyCeiling > defaultMaxKeys {
		t.Errorf("rewrite ceiling %d is above the server's key cap %d: dead configuration",
			core.RewriteKeyCeiling, defaultMaxKeys)
	}
	if core.RewriteKeyCeiling <= defaultMaxKeys/2 {
		t.Errorf("rewrite ceiling %d is far below the server's key cap %d: legal keyspaces "+
			"could not compact, which is how a log grows without bound", core.RewriteKeyCeiling, defaultMaxKeys)
	}
}

// TestServerReportsItsDefaultEviction: a server started with no eviction flags
// reports LRU and no memory bound, as it did when config held them.
func TestServerReportsItsDefaultEviction(t *testing.T) {
	s := startTestServer(t)
	c, r := connectTest(t, s)
	defer c.Close()
	info := call(t, c, r, "INFO", "memory")
	if !strings.Contains(info, "maxmemory:0\r\nmaxmemory_human:0B\r\nmaxmemory_policy:allkeys-lru\r\n") {
		t.Fatalf("INFO memory: %q", info)
	}
	s.stop(t)
}

// TestServerEnforcesMaxKeysAtStartup: the key cap reaches the engine through
// runServer's own Configure, not only through this file's: a server started
// with -maxkeys 1 holds one key.
func TestServerEnforcesMaxKeysAtStartup(t *testing.T) {
	s := startTestServer(t, "-maxkeys", "1")
	c, r := connectTest(t, s)
	defer c.Close()
	for _, key := range []string{"first", "second"} {
		if got := call(t, c, r, "SET", key, "v"); got != "+OK" {
			t.Fatalf("SET %s: %s", key, got)
		}
	}
	if got := call(t, c, r, "DBSIZE"); got != ":1" {
		t.Fatalf("DBSIZE = %s, want :1", got)
	}
	s.stop(t)
}

// TestFlagsReachTheServer: the listener and transport flags reach the server's
// options, each passed explicitly, so that none of the server's settings is a
// server.Options default; with no flags they are the settings the server has
// always had.
func TestFlagsReachTheServer(t *testing.T) {
	configureFrom(t)
	want := server.Options{Host: "127.0.0.1", Port: 8081, MaxClients: 20000, IOThreads: 1,
		CronInterval: 100 * time.Millisecond}
	if got := serverOptions(); got != want {
		t.Fatalf("default flags give the server %+v, want %+v", got, want)
	}
	if got := serverOptions().WithDefaults(); got != want {
		t.Fatalf("the defaults change the server's settings to %+v, want %+v", got, want)
	}

	t.Setenv("KEEL_OPTIONS_TEST_PASSWORD", "secret")
	configureFrom(t, "-host", "0.0.0.0", "-port", "7000", "-maxclients", "12", "-io-threads", "3",
		"-cron-interval-ms", "25", "-requirepass-env", "KEEL_OPTIONS_TEST_PASSWORD",
		"-aof-async-append", "-appendonly", "-aof-concurrent-append", "-primary-tls")
	want = server.Options{Host: "0.0.0.0", Port: 7000, MaxClients: 12, IOThreads: 3,
		CronInterval: 25 * time.Millisecond, RequirePass: "secret", ConcurrentAppend: true, PrimaryTLS: true}
	if got := serverOptions(); got != want {
		t.Fatalf("flags give the server %+v, want %+v", got, want)
	}
}
