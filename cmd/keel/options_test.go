package main

import (
	"errors"
	"flag"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/brandopakel/keel/internal/core"
	"github.com/brandopakel/keel/internal/data_structure"
	"github.com/brandopakel/keel/internal/server"
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
// options rather than into config, except the key bound: LRU, no key bound
// (Redis's default, chosen by the owner on 2026-10-05 over the 5,000,000-key
// cap), no memory bound, and the sampling, LFU and LCS figures; active expiry
// at 20 keys a round, 25% and 16 rounds; no log, at ./keel-master.aof under everysec
// when there is one, appended on the caller's thread and rewritten at 100%
// growth past 64 MiB; and neither a replica nor a feed, in protocol 1.
func TestDefaultFlagsKeepTheServerSettings(t *testing.T) {
	// Not parallel: it parses the process's command line into the default
	// engine's options, which every test in the process shares.
	got := configureFrom(t)
	want := data_structure.Limits{Eviction: data_structure.EvictLRU, MaxKeys: 0, MaxMemory: 0,
		EvictionSamples: 5, LFULogFactor: 10, LFUDecayPeriod: 10000, LCSMaxCells: 134217728}
	if got != want {
		t.Fatalf("default flags hold the engine to %+v, want %+v", got, want)
	}
	wantOptions := core.Options{MaxKeys: 0, Eviction: core.EvictLRU, EvictionSamples: 5,
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
	// Not parallel: it parses the process's command line into the default
	// engine's options, which every test in the process shares.
	got := configureFrom(t, "-maxmemory", "2mb", "-maxkeys", "7", "-evict", "lfu", "-lru-samples", "9",
		"-lfu-log-factor", "3", "-lfu-decay-period", "40", "-lcs-max-cells", "1000")
	want := data_structure.Limits{Eviction: data_structure.EvictLFU, MaxKeys: 7, MaxMemory: 2 << 20,
		EvictionSamples: 9, LFULogFactor: 3, LFUDecayPeriod: 40, LCSMaxCells: 1000}
	if got != want {
		t.Fatalf("flags hold the engine to %+v, want %+v", got, want)
	}

	got = configureFrom(t, "-evict", "random", "-lfu-log-factor", "0", "-lfu-decay-period", "0", "-lcs-max-cells", "0")
	want = data_structure.Limits{Eviction: data_structure.EvictRandom, MaxKeys: 0,
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

// TestServerHasNoKeyBoundByDefault: as in Redis, the server bounds its
// keyspace only when -maxkeys or -maxmemory says so. Until 2026-10-05 the
// default was 5,000,000 keys; the owner chose Redis's default instead.
func TestServerHasNoKeyBoundByDefault(t *testing.T) {
	// Not parallel: it parses the process's command line into the default
	// engine's options, which every test in the process shares.
	if got := configureFrom(t).MaxKeys; got != 0 {
		t.Fatalf("the default -maxkeys gives the engine a key bound of %d, want none", got)
	}
}

// TestServerWarnsWhenItsLogMayOutgrowARewrite: a log rewrite refuses a keyspace
// over core.RewriteKeyCeiling keys, so a server that logs its writes says so at
// startup unless -maxkeys holds the count at or below the ceiling. -maxmemory
// bounds bytes rather than keys, so it does not; and with no log there is
// nothing to compact.
func TestServerWarnsWhenItsLogMayOutgrowARewrite(t *testing.T) {
	t.Parallel()
	ceiling := strconv.Itoa(core.RewriteKeyCeiling)
	for _, c := range []struct {
		name string
		args []string
		warn bool
	}{
		{"log and no bound", []string{"-appendonly"}, true},
		{"log and -maxkeys below the ceiling", []string{"-appendonly", "-maxkeys", "1000"}, false},
		{"log and -maxkeys at the ceiling", []string{"-appendonly", "-maxkeys", ceiling}, false},
		{"log and -maxkeys above the ceiling", []string{"-appendonly", "-maxkeys", "5000000"}, true},
		{"log and only -maxmemory", []string{"-appendonly", "-maxmemory", "64mb"}, true},
		{"no log", nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			args := append([]string{"-appendfilename", filepath.Join(t.TempDir(), "unbounded.aof")}, c.args...)
			s := startTestServer(t, args...)
			s.stop(t)
			if got := strings.Contains(s.log.String(), rewriteCeilingWarning()); got != c.warn {
				t.Fatalf("warning printed: %v, want %v; server log:\n%s", got, c.warn, s.log.String())
			}
		})
	}
}

// TestServerReportsItsDefaultEviction: a server started with no eviction flags
// reports LRU and no memory bound, as it did when config held them.
func TestServerReportsItsDefaultEviction(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	// Not parallel: it parses the process's command line into the default
	// engine's options, which every test in the process shares.
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

// developFlags is every flag the server had before step 2.5 of the embedding
// plan replaced internal/config with options, with its type and its default,
// as the build at d56088a lists them in -h. Step 2.5 promised that no flag
// changes its name, its type or its default; the parts of the step that moved
// the settings behind the flags have to keep that promise, and so does
// anything after them that means to.
var developFlags = []struct{ name, kind, value string }{
	{"active-expire-samples", "int", "20"},
	{"aof-async-append", "", "false"},
	{"aof-concurrent-append", "", "false"},
	{"appendfilename", "string", "./keel-master.aof"},
	{"appendfsync", "string", "everysec"},
	{"appendonly", "", "false"},
	{"auto-aof-rewrite-min-size", "string", "64mb"},
	{"auto-aof-rewrite-percentage", "int", "100"},
	{"cron-interval-ms", "int", "100"},
	{"evict", "string", "lru"},
	{"host", "string", "127.0.0.1"},
	{"io-threads", "int", "1"},
	{"lcs-max-cells", "uint", "134217728"},
	{"lfu-decay-period", "int", "10000"},
	{"lfu-log-factor", "int", "10"},
	{"lru-samples", "int", "5"},
	{"maxclients", "int", "20000"},
	// The one deliberate change since d56088a: the owner chose Redis's
	// unbounded default over the 5,000,000-key cap on 2026-10-05.
	{"maxkeys", "int", "0"},
	{"maxmemory", "string", "0"},
	{"mode", "string", "kqueue"},
	{"port", "int", "8081"},
	{"primary-password-env", "string", ""},
	{"primary-tls", "", "false"},
	{"profile-dir", "string", ""},
	{"replicaof", "string", ""},
	{"replication-feed", "", "false"},
	{"replication-protocol", "int", "1"},
	{"requirepass-env", "string", ""},
	{"shutdown-timeout", "duration", "5s"},
	{"version", "", "false"},
}

// TestFlagsKeepTheirNamesAndDefaults reads the flags parseFlags defines and
// requires exactly develop's: the same names, each of the same type with the
// same default, and no flag added or taken away. What each flag does is
// checked by the tests above, which map the defaults and every flag onto the
// engine's and the server's options, and by the subprocess tests, which start
// the server through main.
func TestFlagsKeepTheirNamesAndDefaults(t *testing.T) {
	// Not parallel: it parses the process's command line into the default
	// engine's options, which every test in the process shares.
	configureFrom(t)
	got := map[string][2]string{}
	flag.CommandLine.VisitAll(func(f *flag.Flag) {
		kind, _ := flag.UnquoteUsage(f)
		got[f.Name] = [2]string{kind, f.DefValue}
	})
	for _, want := range developFlags {
		have, ok := got[want.name]
		if !ok {
			t.Errorf("-%s is gone", want.name)
			continue
		}
		if have != [2]string{want.kind, want.value} {
			t.Errorf("-%s is %q with default %q, want %q with default %q", want.name, have[0], have[1], want.kind, want.value)
		}
		delete(got, want.name)
	}
	for name := range got {
		t.Errorf("-%s is new", name)
	}
}

// TestServerStartsWithItsListenerAndTransportFlags starts the server through
// main, as an operator does, with every listener and transport flag that
// server.Options carries set to something other than its default. The
// address (as in every subprocess test), the password and a two-connection
// limit are checked by what the server does with them; with two I/O threads,
// a 10 ms cron and concurrent appends on a worker it has to serve, and reap a
// key nobody reads. TestFlagsReachTheServer checks the values those flags pass.
func TestServerStartsWithItsListenerAndTransportFlags(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "flags.aof")
	s := startTestServer(t, "-maxclients", "2", "-io-threads", "2", "-cron-interval-ms", "10",
		"-requirepass-env", "KEEL_TEST_PASSWORD", "-appendonly", "-appendfilename", path,
		"-aof-async-append", "-aof-concurrent-append")
	c, r := connectTest(t, s)
	if got := call(t, c, r, "SET", "k", "v"); !strings.HasPrefix(got, "-NOAUTH") {
		t.Fatalf("SET before AUTH: %s", got)
	}
	if got := call(t, c, r, "AUTH", "integration-secret"); got != "+OK" {
		t.Fatalf("AUTH: %s", got)
	}
	if got := call(t, c, r, "SET", "brief", "v", "PX", "20"); got != "+OK" {
		t.Fatalf("SET: %s", got)
	}
	// With nothing else to do, only the cron wakes the loop to reap it.
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(call(t, c, r, "INFO", "stats"), "expired_keys:1\r\n") {
		if time.Now().After(deadline) {
			t.Fatal("the idle key was not reaped on the cron's clock")
		}
		time.Sleep(25 * time.Millisecond)
	}

	// A second connection is the last one -maxclients 2 admits; a third is
	// accepted and closed at once.
	second, secondReader := connectTest(t, s)
	if got := call(t, second, secondReader, "AUTH", "integration-secret"); got != "+OK" {
		t.Fatalf("second connection: %s", got)
	}
	third, err := net.DialTimeout("tcp", s.addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer third.Close()
	third.SetDeadline(time.Now().Add(3 * time.Second))
	third.Write([]byte(request("PING")))
	n, err := third.Read(make([]byte, 64))
	var timeout net.Error
	if err == nil || (errors.As(err, &timeout) && timeout.Timeout()) {
		t.Fatalf("a third connection past -maxclients 2 read %d bytes and %v, want it closed", n, err)
	}
	if got := call(t, c, r, "PING"); got != "+PONG" {
		t.Fatalf("the first connection after the refusal: %s", got)
	}
	s.stop(t)
}
