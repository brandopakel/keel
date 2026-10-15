package core

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// configPairs runs CONFIG GET and reads its flat RESP2 reply into name and
// value pairs, in order.
func configPairs(t testing.TB, e *Engine, patterns ...string) [][2]string {
	t.Helper()
	reply, ok := runOn(t, e, "CONFIG", append([]string{"GET"}, patterns...)...).([]interface{})
	require.True(t, ok, "CONFIG GET answers an array")
	require.Zero(t, len(reply)%2)
	var pairs [][2]string
	for i := 0; i < len(reply); i += 2 {
		pairs = append(pairs, [2]string{reply[i].(string), reply[i+1].(string)})
	}
	return pairs
}

// TestConfigGetReportsKeelsSettingsUnderRedisNames: the settings Keel has,
// each under Redis's name and in Redis's format, read live from the engine.
func TestConfigGetReportsKeelsSettingsUnderRedisNames(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{MaxMemory: 1 << 20, Eviction: EvictLFU})
	for name, want := range map[string]string{
		"maxmemory": "1048576", "maxmemory-policy": "allkeys-lfu", "maxmemory-samples": "5",
		"lfu-log-factor": "10", "appendonly": "no", "appendfsync": "everysec",
		"auto-aof-rewrite-percentage": "100", "auto-aof-rewrite-min-size": "67108864",
		"databases": "1", "save": "", "replicaof": "", "slaveof": "",
	} {
		assert.Equal(t, [][2]string{{name, want}}, configPairs(t, e, name), name)
	}
	dir, err := os.Getwd()
	require.NoError(t, err)
	assert.Equal(t, [][2]string{{"dir", dir}}, configPairs(t, e, "dir"))

	reconfigure(t, e, func(o *Options) { o.MaxMemory = 2 << 20; o.Eviction = EvictRandom })
	assert.Equal(t, [][2]string{{"maxmemory", "2097152"}, {"maxmemory-policy", "allkeys-random"}},
		configPairs(t, e, "maxmemory", "maxmemory-policy"), "the live values")
}

// TestConfigGetMatchesAsRedisDoes: names without regard to case, spelled
// back as asked; glob patterns with Redis's spelling, aliases included and
// hidden directives left out; each name once; nothing for a name Keel does
// not answer for.
func TestConfigGetMatchesAsRedisDoes(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.Equal(t, [][2]string{{"MAXMEMORY", "0"}}, configPairs(t, e, "MAXMEMORY"))
	assert.ElementsMatch(t, [][2]string{{"maxmemory", "0"}, {"maxmemory-policy", "allkeys-lru"}, {"maxmemory-samples", "5"},
		{"maxmemory-eviction-tenacity", "10"}, {"maxmemory-clients", "0"}}, configPairs(t, e, "MAXMEM*"))
	assert.Equal(t, [][2]string{{"appendonly", "no"}, {"maxmemory", "0"}},
		configPairs(t, e, "appendonly", "maxmemory", "maxmemory", "append?nly", "nosuch"))
	assert.Empty(t, configPairs(t, e, "nosuch", "lfu-decay-time"), "a name Keel does not answer for")
	assert.Equal(t, [][2]string{{"replicaof", ""}, {"slaveof", ""}}, configPairs(t, e, "[rs]*of"))
	assert.Equal(t, [][2]string{{"hash-max-listpack-entries", "512"}, {"hash-max-ziplist-entries", "512"}},
		configPairs(t, e, "hash-max-[lz]*-entries"), "a name and its alias")
	assert.Equal(t, [][2]string{{"hash-max-ziplist-entries", "512"}, {"hash-max-listpack-entries", "512"}},
		configPairs(t, e, "hash-max-ziplist-entries", "hash-max-listpack-entries"), "both, each as asked")
	assert.Empty(t, configPairs(t, e, "lazyexpire-*"), "a hidden directive is matched by name alone")
	assert.Equal(t, [][2]string{{"lazyexpire-nested-arbitrary-keys", "yes"}}, configPairs(t, e, "lazyexpire-nested-arbitrary-keys"))
}

// TestConfigGetReportsTheTransportsSettingsOnceItHasSaid: port, bind and the
// rest come from the transport, through SetServerInfo, and are absent without.
func TestConfigGetReportsTheTransportsSettingsOnceItHasSaid(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.Empty(t, configPairs(t, e, "port", "bind", "maxclients", "hz", "requirepass"))
	e.SetServerInfo(&ServerInfo{Port: 6390, MaxClients: 50, TCPBacklog: 511, IOThreads: 4, Hz: 10, Multiplexer: "epoll",
		Host: "0.0.0.0", RequirePass: "secret"})
	got := map[string]string{}
	for _, p := range configPairs(t, e, "*") {
		got[p[0]] = p[1]
	}
	for name, want := range map[string]string{"port": "6390", "bind": "0.0.0.0", "maxclients": "50",
		"tcp-backlog": "511", "io-threads": "4", "hz": "10", "requirepass": "secret"} {
		assert.Equal(t, want, got[name], name)
	}
}

// TestConfigReplicaofIsWrittenAsRedisWritesIt: host and port separated by a
// space, as Redis writes a primary.
func TestConfigReplicaofIsWrittenAsRedisWritesIt(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	e.role.ReplicaOf = "10.0.0.1:6380"
	assert.Equal(t, [][2]string{{"replicaof", "10.0.0.1 6380"}, {"slaveof", "10.0.0.1 6380"}}, configPairs(t, e, "replicaof", "slaveof"))
}

// TestConfigRepliesInEitherProtocol: a flat array in RESP2 and a map in RESP3,
// as Redis answers; HELP and the refusals in Redis's words.
func TestConfigRepliesInEitherProtocol(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.Equal(t, "*2\r\n$9\r\nmaxmemory\r\n$1\r\n0\r\n", string(rawReplyAsOn(t, e, false, "CONFIG", "GET", "maxmemory")))
	assert.Equal(t, "%1\r\n$9\r\nmaxmemory\r\n$1\r\n0\r\n", string(rawReplyAsOn(t, e, true, "CONFIG", "GET", "maxmemory")))
	assert.Equal(t, "*0\r\n", string(rawReplyOn(t, e, "CONFIG", "GET", "nosuch")))

	assert.Equal(t, []interface{}{"CONFIG <subcommand> [<arg> [value] [opt] ...]. Subcommands are:",
		"GET <pattern>", "    Return parameters matching the glob-like <pattern> and their values.",
		"SET <directive> <value>", "    Set the configuration <directive> to <value>.",
		"RESETSTAT", "    Reset statistics reported by the INFO command.",
		"REWRITE", "    Rewrite the configuration file.",
		"HELP", "    Print this help."}, runOn(t, e, "CONFIG", "HELP"))
	for args, want := range map[string]string{
		"":              "-ERR wrong number of arguments for 'config' command\r\n",
		"GET":           "-ERR wrong number of arguments for 'config|get' command\r\n",
		"SET a":         "-ERR wrong number of arguments for 'config|set' command\r\n",
		"resetstat x":   "-ERR wrong number of arguments for 'config|resetstat' command\r\n",
		"nosuch":        "-ERR unknown subcommand 'nosuch'. Try CONFIG HELP.\r\n",
		"HELP extra":    "-ERR wrong number of arguments for 'config|help' command\r\n",
		"REWRITE":       "-ERR The server is running without a config file\r\n",
		"rewrite extra": "-ERR wrong number of arguments for 'config|rewrite' command\r\n",
	} {
		assert.Equal(t, want, string(rawReplyOn(t, e, "CONFIG", strings.Fields(args)...)), args)
	}
}

// configSet runs CONFIG SET with the given arguments and returns its reply.
func configSet(t testing.TB, e *Engine, args string) string {
	t.Helper()
	return string(rawReplyOn(t, e, "CONFIG", append([]string{"SET"}, strings.Fields(args)...)...))
}

// configError is Redis's error for a CONFIG SET that failed for name.
func configError(name, reason string) string {
	return "-ERR CONFIG SET failed (possibly related to argument '" + name + "') - " + reason + "\r\n"
}

// TestConfigSetChangesKeelsSettingsAtOnce: maxmemory, the eviction policy and
// its samples, the LFU factor, the log's fsync and its automatic rewrite all
// take a new value at once, and CONFIG GET and the engine read it.
func TestConfigSetChangesKeelsSettingsAtOnce(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	require.Equal(t, "+OK\r\n", configSet(t, e, "maxmemory 1mb maxmemory-policy ALLKEYS-LFU maxmemory-samples 10 lfu-log-factor 0"))
	assert.Equal(t, uint64(1<<20), e.Limits().MaxMemory)
	assert.Equal(t, EvictLFU, e.Limits().Eviction)
	assert.Equal(t, 10, e.Limits().EvictionSamples)
	assert.Equal(t, 0, e.Limits().LFULogFactor)
	assert.ElementsMatch(t, [][2]string{{"maxmemory", "1048576"}, {"maxmemory-policy", "allkeys-lfu"},
		{"maxmemory-samples", "10"}, {"lfu-log-factor", "0"}},
		configPairs(t, e, "maxmemory", "maxmemory-policy", "maxmemory-samples", "lfu-log-factor"))

	require.Equal(t, "+OK\r\n", configSet(t, e, "appendfsync always auto-aof-rewrite-percentage 0 auto-aof-rewrite-min-size 1kb"))
	assert.Equal(t, FsyncAlways, e.settings.fsync)
	assert.Equal(t, 0, e.settings.rewritePercentage, "zero is never")
	assert.Equal(t, int64(1024), e.settings.rewriteMinSize)
	assert.Equal(t, [][2]string{{"appendfsync", "always"}, {"auto-aof-rewrite-percentage", "0"}, {"auto-aof-rewrite-min-size", "1024"}},
		configPairs(t, e, "appendfsync", "auto-aof-rewrite-percentage", "auto-aof-rewrite-min-size"))

	require.Equal(t, "+OK\r\n", configSet(t, e, "maxmemory 0 lfu-log-factor 7"))
	assert.Equal(t, uint64(0), e.Limits().MaxMemory)
	assert.Equal(t, 7, e.Limits().LFULogFactor)
}

// keelPolicies is how CONFIG SET refuses a policy of Redis's that Keel does
// not have (volatile-lrm, allkeys-lrm): Redis's words, naming Keel's.
const keelPolicies = "argument(s) must be one of the following: volatile-lru, volatile-lfu, volatile-random, volatile-ttl, " +
	"allkeys-lru, allkeys-lfu, allkeys-random, noeviction"

// TestConfigSetReadsValuesAsRedisDoes: each type's errors, word for word, and
// the directive named as Redis spells it, its alias if that is what was set.
func TestConfigSetReadsValuesAsRedisDoes(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	for args, want := range map[string]string{
		"activerehashing maybe":          configError("activerehashing", "argument must be 'yes' or 'no'"),
		"maxmemory 1zb":                  configError("maxmemory", "argument must be a memory value"),
		"maxmemory -1":                   configError("maxmemory", "argument must be a memory value"),
		"maxmemory-samples 0":            configError("maxmemory-samples", "argument must be between 1 and 64 inclusive"),
		"maxmemory-samples +5":           configError("maxmemory-samples", "argument couldn't be parsed into an integer"),
		"MAXMEMORY-SAMPLES 05":           configError("maxmemory-samples", "argument couldn't be parsed into an integer"),
		"hash-max-ziplist-entries x":     configError("hash-max-ziplist-entries", "argument couldn't be parsed into an integer"),
		"hash-max-listpack-entries -1":   configError("hash-max-listpack-entries", "argument must be between 0 and 9223372036854775807 inclusive"),
		"set-max-intset-entries 1e3":     configError("set-max-intset-entries", "argument couldn't be parsed into an integer"),
		"maxmemory-policy lru":           configError("maxmemory-policy", "argument(s) must be one of the following: volatile-lru, volatile-lfu, volatile-random, volatile-ttl, volatile-lrm, allkeys-lru, allkeys-lfu, allkeys-random, allkeys-lrm, noeviction"),
		"maxmemory-policy volatile-lrm":  configError("maxmemory-policy", keelPolicies),
		"maxmemory-policy allkeys-lrm":   configError("maxmemory-policy", keelPolicies),
		"appendfsync sometimes":          configError("appendfsync", "argument(s) must be one of the following: everysec, always, no"),
		"maxmemory-clients 200%":         configError("maxmemory-clients", "percentage argument must be less or equal to 100"),
		"maxmemory-clients x%":           configError("maxmemory-clients", "argument must be a memory or percent value"),
		"array-slice-size 300":           configError("array-slice-size", "array-slice-size must be a power of two"),
		"array-sparse-kmax 5":            configError("array-sparse-kmax", "array-sparse-kmax must be greater than array-sparse-kmin when non-zero"),
		"array-sparse-kmin 10":           configError("array-sparse-kmin", "array-sparse-kmin must be less than array-sparse-kmax"),
		"cluster-slot-stats-enabled cpu": "+OK\r\n",
		"repl-diskless-load nosuch":      configError("repl-diskless-load", "argument(s) must be one of the following: disabled, on-empty-db, swapdb, flushdb"),
	} {
		assert.Equal(t, want, configSet(t, e, args), args)
	}
	assert.Equal(t, [][2]string{{"maxmemory", "0"}}, configPairs(t, e, "maxmemory"), "no refused value was set")
	assert.Equal(t, [][2]string{{"cluster-slot-stats-enabled", "cpu"}}, configPairs(t, e, "cluster-slot-stats-enabled"))
	require.Equal(t, "+OK\r\n", configSet(t, e, "cluster-slot-stats-enabled yes"))
	assert.Equal(t, [][2]string{{"cluster-slot-stats-enabled", "yes"}}, configPairs(t, e, "cluster-slot-stats-enabled"),
		"bit flags written back as Redis names them")
}

// TestConfigSetKeepsRedissInternalSettings: a setting that only tunes Redis's
// internals is validated, kept and reported, under its name and its alias,
// and changes nothing in Keel.
func TestConfigSetKeepsRedissInternalSettings(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	require.Equal(t, "+OK\r\n", configSet(t, e, "hash-max-ziplist-entries 128 list-max-ziplist-size -1 hll-sparse-max-bytes 1kb lazyfree-lazy-user-del YES"))
	assert.ElementsMatch(t, [][2]string{{"hash-max-listpack-entries", "128"}, {"hash-max-ziplist-entries", "128"},
		{"list-max-listpack-size", "-1"}, {"hll-sparse-max-bytes", "1024"}, {"lazyfree-lazy-user-del", "yes"}},
		configPairs(t, e, "hash-max-listpack-entries", "hash-max-ziplist-entries", "list-max-listpack-size", "hll-sparse-max-bytes", "lazyfree-lazy-user-del"))
	require.Equal(t, "+OK\r\n", configSet(t, e, "array-sparse-kmax 0 array-sparse-kmin 100"), "kmin is checked against kmax as it is when kmin is read")
}

// TestConfigSetRefusesWhatKeelCannotChange: a setting Keel has but cannot
// change while it runs, or a feature it lacks whose off value is the truth,
// takes the value it has and refuses any other as Redis refuses a directive it
// cannot set; one Redis cannot set at all is refused as Redis refuses it,
// before any value is read.
func TestConfigSetRefusesWhatKeelCannotChange(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	for args, want := range map[string]string{
		"appendonly no":                "+OK\r\n",
		"appendonly yes":               configError("appendonly", "can't set immutable config"),
		"notify-keyspace-events KEA":   configError("notify-keyspace-events", "can't set immutable config"),
		"notify-keyspace-events Q":     configError("notify-keyspace-events", "Invalid event class character. Use 'Ag$lshzxeKEtmdnocaSTIV'."),
		"maxmemory-clients 0":          "+OK\r\n",
		"maxmemory-clients 10%":        configError("maxmemory-clients", "can't set immutable config"),
		"proto-max-bulk-len 536870912": "+OK\r\n",
		"proto-max-bulk-len 10gb":      configError("proto-max-bulk-len", "can't set immutable config"),
		"databases 2":                  configError("databases", "can't set immutable config"),
		"Databases 1":                  configError("Databases", "can't set immutable config"),
		"slaveof x":                    configError("slaveof", "can't set immutable config"),
		"dir /tmp":                     configError("dir", "can't set protected config"),
		"daemonize yes":                configError("daemonize", "can't set immutable config"),
		"dbfilename x":                 configError("dbfilename", "can't set protected config"),
		"timeout 10":                   "-ERR Unknown option or number of arguments for CONFIG SET - 'timeout'\r\n",
		"port 6000":                    "-ERR Unknown option or number of arguments for CONFIG SET - 'port'\r\n",
		"req-res-logfile x":            "-ERR Unknown option or number of arguments for CONFIG SET - 'req-res-logfile'\r\n",
		"repl-compression 1":           "-ERR Unknown option or number of arguments for CONFIG SET - 'repl-compression'\r\n",
	} {
		assert.Equal(t, want, configSet(t, e, args), args)
	}
	// An empty value is one argument: save "".
	assert.Equal(t, "+OK\r\n", string(rawReplyOn(t, e, "CONFIG", "SET", "save", "")))
	assert.Equal(t, configError("save", "can't set immutable config"), string(rawReplyOn(t, e, "CONFIG", "SET", "save", "900 1")))
	assert.Equal(t, configError("save", "Invalid save parameters"), string(rawReplyOn(t, e, "CONFIG", "SET", "save", "900")))
	assert.Equal(t, configError("save", "Invalid save parameters"), string(rawReplyOn(t, e, "CONFIG", "SET", "save", "0 1")))
	assert.Equal(t, "+OK\r\n", string(rawReplyOn(t, e, "CONFIG", "SET", "notify-keyspace-events", "")))
	assert.Equal(t, "+OK\r\n", string(rawReplyOn(t, e, "CONFIG", "SET", "latency-tracking-info-percentiles", "50.0 99 99.900")),
		"the percentiles Keel has, written another way")
	assert.Equal(t, configError("latency-tracking-info-percentiles", "can't set immutable config"),
		string(rawReplyOn(t, e, "CONFIG", "SET", "latency-tracking-info-percentiles", "50 99")))
	assert.Equal(t, configError("latency-tracking-info-percentiles", "Invalid latency-tracking-info-percentiles parameters"),
		string(rawReplyOn(t, e, "CONFIG", "SET", "latency-tracking-info-percentiles", "50 x")))
	assert.Equal(t, configError("latency-tracking-info-percentiles", "latency-tracking-info-percentiles parameters should sit between [0.0,100.0]"),
		string(rawReplyOn(t, e, "CONFIG", "SET", "latency-tracking-info-percentiles", "101")))
	assert.Equal(t, "+OK\r\n", string(rawReplyOn(t, e, "CONFIG", "SET", "latency-monitor-threshold", "200")),
		"the latency monitor's, live (watch_latency.go)")
	assert.Equal(t, [][2]string{{"latency-tracking", "yes"}, {"latency-tracking-info-percentiles", "50 99 99.9"}, {"latency-monitor-threshold", "200"}},
		configPairs(t, e, "latency-tracking", "latency-tracking-info-percentiles", "latency-monitor-threshold"))
}

// TestConfigSetIsAllOrNothing: Redis's order of refusals - a name it does not
// know, unless a pair before it already failed; a directive it cannot set; one
// given twice - then each value read in order, and only then any change, put
// back if a later one fails.
func TestConfigSetIsAllOrNothing(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	for args, want := range map[string]string{
		"nosuch 1 maxmemory 1":                                   "-ERR Unknown option or number of arguments for CONFIG SET - 'nosuch'\r\n",
		"databases 1 nosuch 1":                                   configError("databases", "can't set immutable config"),
		"maxmemory 1 nosuch 1 other 2":                           "-ERR Unknown option or number of arguments for CONFIG SET - 'nosuch'\r\n",
		"maxmemory 1 MAXMEMORY 2":                                configError("MAXMEMORY", "duplicate parameter"),
		"hash-max-listpack-entries 1 hash-max-ziplist-entries 2": configError("hash-max-ziplist-entries", "duplicate parameter"),
		"maxmemory 1mb maxmemory-samples 100":                    configError("maxmemory-samples", "argument must be between 1 and 64 inclusive"),
		"maxmemory 1 hz":                                         "-ERR syntax error\r\n",
	} {
		assert.Equal(t, want, configSet(t, e, args), args)
	}
	assert.Equal(t, [][2]string{{"maxmemory", "0"}}, configPairs(t, e, "maxmemory"), "nothing was set")

	// A change that fails puts back every one made before it.
	e.SetServerInfo(&ServerInfo{MaxClients: 10, SetMaxClients: func(n int) string {
		return "The operating system is not able to handle the specified number of clients, try with 5"
	}})
	assert.Equal(t, configError("maxclients", "The operating system is not able to handle the specified number of clients, try with 5"),
		configSet(t, e, "maxmemory 1mb hash-max-listpack-entries 7 maxclients 100"))
	assert.Equal(t, uint64(0), e.Limits().MaxMemory)
	assert.Equal(t, [][2]string{{"maxmemory", "0"}, {"hash-max-listpack-entries", "512"}, {"maxclients", "10"}},
		configPairs(t, e, "maxmemory", "hash-max-listpack-entries", "maxclients"))
}

// TestConfigSetChangesTheTransportsSettings: the password, the clock's rate
// and the most clients go to the transport, which applies them, and CONFIG
// GET reports what it now has; hz out of Redis's range is taken at its nearest
// end. Without a transport, they are names Keel does not know.
func TestConfigSetChangesTheTransportsSettings(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.Equal(t, "-ERR Unknown option or number of arguments for CONFIG SET - 'requirepass'\r\n", configSet(t, e, "requirepass x"))
	var password string
	var hz, clients int
	e.SetServerInfo(&ServerInfo{Port: 6379, MaxClients: 10, Hz: 10,
		SetRequirePass: func(p string) { password = p },
		SetHz:          func(n int) { hz = n },
		SetMaxClients:  func(n int) string { clients = n; return "" }})
	require.Equal(t, "+OK\r\n", configSet(t, e, "requirepass secret hz 0 maxclients 50"))
	assert.Equal(t, "secret", password)
	assert.Equal(t, 1, hz)
	assert.Equal(t, 50, clients)
	assert.Equal(t, [][2]string{{"requirepass", "secret"}, {"hz", "1"}, {"maxclients", "50"}},
		configPairs(t, e, "requirepass", "hz", "maxclients"))
	require.Equal(t, "+OK\r\n", configSet(t, e, "hz 100000"))
	assert.Equal(t, 500, hz)
	assert.Equal(t, configError("maxclients", "argument must be between 1 and 4294967295 inclusive"), configSet(t, e, "maxclients 0"))
	assert.Equal(t, "+OK\r\n", configSet(t, e, "port 6379"))
	assert.Equal(t, configError("port", "can't set immutable config"), configSet(t, e, "port 6380"))
	assert.Equal(t, configError("tcp-backlog", "can't set immutable config"), configSet(t, e, "tcp-backlog 1"))
}

// TestConfigSetChangesTheSlowLogsSettings: the slow log's four settings
// change at once, are read back, and keep Redis's bounds; a shorter log is
// cut when the next entry joins it.
func TestConfigSetChangesTheSlowLogsSettings(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	require.Equal(t, "OK", runOn(t, e, "CONFIG", "SET", "slowlog-log-slower-than", "0", "slowlog-max-len", "5",
		"slowlog-entry-max-argc", "3", "slowlog-entry-max-string-len", "4"))
	assert.Equal(t, [][2]string{{"slowlog-entry-max-argc", "3"}, {"slowlog-max-len", "5"},
		{"slowlog-entry-max-string-len", "4"}, {"slowlog-log-slower-than", "0"}}, configPairs(t, e, "slowlog-*"),
		"in the order of Redis's table")
	runOn(t, e, "SET", "key", "a-long-value")
	entries := slowlogGet(t, e)
	require.NotEmpty(t, entries)
	assert.Equal(t, []interface{}{"SET", "key", "a-lo... (8 more bytes)"}, entries[0][3], "cut as the settings say")

	runOn(t, e, "CONFIG", "SET", "slowlog-max-len", "1")
	runOn(t, e, "PING")
	assert.Equal(t, int64(1), runOn(t, e, "SLOWLOG", "LEN"))

	assert.Equal(t, configError("slowlog-entry-max-argc", "argument must be between 2 and 2147483647 inclusive"),
		configSet(t, e, "slowlog-entry-max-argc 1"))
	assert.Equal(t, configError("slowlog-log-slower-than", "argument must be between -1 and 9223372036854775807 inclusive"),
		configSet(t, e, "slowlog-log-slower-than -2"))
}
