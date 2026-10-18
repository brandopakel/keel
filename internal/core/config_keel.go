package core

import (
	"math"
	"strconv"
	"strings"
	"sync"
)

// Which of Redis's configuration directives Keel answers CONFIG GET and SET
// for, and how. docs/info-compatibility.md lists them with the reasons.
//
//   - configLive: a setting Keel has. CONFIG SET changes it at once, with
//     Redis's validation, and CONFIG GET reads it from the engine or the
//     transport.
//   - configStored: a setting that only tunes Redis's own internals, such as
//     the size at which a hash leaves its compact encoding. Nothing a client
//     sees depends on it beyond OBJECT ENCODING, and Keel has nothing it
//     would tune. CONFIG SET validates a value as Redis does and keeps it, and
//     CONFIG GET reports it, so that tools and test suites that set one can
//     run; it changes nothing in Keel.
//   - configFixed: a setting Keel has but cannot change while it runs, or a
//     feature it lacks whose off value is the truth about Keel (no snapshots,
//     no keyspace notifications). CONFIG GET reports Keel's value, and CONFIG
//     SET accepts that value, as Redis accepts setting a directive to the
//     value it has, and refuses any other in Redis's words for a directive it
//     cannot set.
//
// A directive of Redis's table not listed here is one whose being ignored
// would change what a client relies on (min-replicas-to-write,
// client-output-buffer-limit, timeout, TLS, ACL, ...): Keel answers it as
// Redis answers a name it does not know.
type configHandling uint8

const (
	configLive configHandling = iota + 1
	configStored
	configFixed
)

// keelConfig is how Keel answers one directive.
type keelConfig struct {
	handling configHandling
	// transport says the setting is the transport's (port, requirepass, ...):
	// Keel answers for it only once a transport has said what it is, through
	// SetServerInfo, as before.
	transport bool
	// get is Keel's value, as CONFIG GET writes it; a stored directive's is
	// the value last set, Redis's default until then.
	get func(e *Engine) string
	// set applies a live directive's value, as read (configSpec.read or
	// read below), and returns Redis's error text, or "" when it applied.
	set func(e *Engine, value string) string
	// read reads a special directive's value: as CONFIG GET would write it,
	// or Redis's error text.
	read func(value string) (string, string)
	// valid is Redis's check of a value against other settings, where the
	// directive has one (its is_valid function). setting reads another
	// directive as this CONFIG SET leaves it so far: Redis sets each pair
	// before it reads the next.
	valid func(setting func(name string) string, value string) string
}

// errImmutableConfig is Redis's reason for refusing a directive it cannot set.
const errImmutableConfig = "can't set immutable config"

// keelConfigs is every directive Keel answers for, by Redis's name, built
// the first time CONFIG asks (configTables). The tables CONFIG reads are
// built in functions of their own, on first use, rather than in package
// initializers: the compiler places the package's init, and an initializer's
// closures, ahead of the command handlers, where their room would move every
// handler in the binary the paired command-path job measures, and places a
// function and its closures after it, here, after the handlers.
var (
	keelConfigs  map[string]keelConfig
	configLookup map[string]*configSpec
	configOnce   sync.Once
)

// configTables builds keelConfigs and configLookup, once.
func configTables() {
	configOnce.Do(func() {
		keelConfigs = keelConfigTable()
		configLookup = configLookupTable()
	})
}

// keelConfigTable builds keelConfigs.
func keelConfigTable() map[string]keelConfig {
	m := map[string]keelConfig{
		// The keyspace's limits and eviction.
		"maxmemory": {handling: configLive,
			get: func(e *Engine) string { return strconv.FormatUint(e.space.Limits().MaxMemory, 10) },
			set: func(e *Engine, v string) string {
				n, _ := strconv.ParseUint(v, 10, 64)
				return e.setOption(func(o *Options) { o.MaxMemory = n })
			}},
		"maxmemory-policy": {handling: configLive,
			get: func(e *Engine) string { return evictionPolicyName(e.space.Limits().Eviction) },
			set: func(e *Engine, v string) string {
				policy, ok := EvictionPolicyNamed(v)
				if !ok {
					// A policy of Redis's that Keel does not have,
					// volatile-lrm or allkeys-lrm, is refused in Redis's
					// words, naming the ones Keel has.
					return "argument(s) must be one of the following: " + evictionPolicyNames()
				}
				refused := e.setOption(func(o *Options) { o.Eviction = policy })
				e.settleEvictionExceeded()
				return refused
			}},
		"maxmemory-samples": {handling: configLive,
			get: func(e *Engine) string { return strconv.Itoa(e.space.Limits().EvictionSamples) },
			set: func(e *Engine, v string) string {
				n, _ := strconv.Atoi(v)
				return e.setOption(func(o *Options) { o.EvictionSamples = n })
			}},
		"lfu-log-factor": {handling: configLive,
			get: func(e *Engine) string { return strconv.Itoa(e.space.Limits().LFULogFactor) },
			set: func(e *Engine, v string) string {
				n, _ := strconv.Atoi(v)
				return e.setOption(func(o *Options) { o.LFULogFactor = zeroIsOff(n) })
			}},

		// The log.
		"appendonly": {handling: configFixed, get: func(e *Engine) string { return yesNo(e.options.AppendOnly) }},
		"appendfilename": {handling: configFixed,
			get: func(e *Engine) string { return e.options.WithDefaults().AppendFilename }},
		"appendfsync": {handling: configLive,
			get: func(e *Engine) string { return string(e.settings.fsync) },
			set: func(e *Engine, v string) string {
				return e.setOption(func(o *Options) { o.Fsync = FsyncPolicy(v) })
			}},
		"auto-aof-rewrite-percentage": {handling: configLive,
			get: func(e *Engine) string { return strconv.Itoa(e.settings.rewritePercentage) },
			set: func(e *Engine, v string) string {
				n, _ := strconv.Atoi(v)
				return e.setOption(func(o *Options) { o.AutoRewritePercentage = zeroIsOff(n) })
			}},
		"auto-aof-rewrite-min-size": {handling: configLive,
			get: func(e *Engine) string { return strconv.FormatInt(e.settings.rewriteMinSize, 10) },
			set: func(e *Engine, v string) string {
				n, _ := strconv.ParseInt(v, 10, 64)
				return e.setOption(func(o *Options) { o.AutoRewriteMinSize = zeroIsOff(n) })
			}},

		// One database, no snapshots, and the working directory.
		"databases": {handling: configFixed, get: func(*Engine) string { return "1" }},
		"save":      {handling: configFixed, get: func(*Engine) string { return "" }, read: readSave},
		"dir":       {handling: configFixed, get: workingDirectory, read: func(v string) (string, string) { return v, "" }},
		// Redis writes the primary as "host port"; Keel takes it as host:port.
		// slaveof is Redis's old name for it, which it still answers to.
		"replicaof": {handling: configFixed, get: func(e *Engine) string {
			primary := e.replicaOf()
			if i := strings.LastIndexByte(primary, ':'); i > 0 {
				primary = primary[:i] + " " + primary[i+1:]
			}
			return primary
		}, read: func(v string) (string, string) { return v, "" }},

		// No keyspace notifications: Redis's value with none turned on.
		"notify-keyspace-events": {handling: configFixed, get: func(*Engine) string { return "" },
			read: readKeyspaceEvents},
		// No eviction of clients for their memory: Redis's 0.
		"maxmemory-clients": {handling: configFixed, get: func(*Engine) string { return "0" }},
		// Every command's latency is tracked, at Redis's default percentiles
		// (latency.go).
		"latency-tracking": {handling: configFixed, get: func(*Engine) string { return "yes" }},
		"latency-tracking-info-percentiles": {handling: configFixed, get: func(*Engine) string {
			labels := make([]string, len(latencyPercentiles))
			for i, p := range latencyPercentiles {
				labels[i] = p.label
			}
			return strings.Join(labels, " ")
		}, read: readPercentiles},
		// The latency monitor's (watch_latency.go), changed at once, as
		// Redis's is.
		"latency-monitor-threshold": {handling: configLive,
			get: func(e *Engine) string { return strconv.FormatInt(e.latencyMonitor.threshold, 10) },
			set: func(e *Engine, v string) string {
				e.latencyMonitor.threshold, _ = strconv.ParseInt(v, 10, 64)
				e.watchLongCommands()
				return ""
			}},
		// The slow log's (slowlog.go), changed at once, as Redis's are; a
		// shorter log is cut back when the next entry joins it, as Redis's is.
		"slowlog-log-slower-than": {handling: configLive,
			get: func(e *Engine) string { return strconv.FormatInt(e.slowlog.slowerThan, 10) },
			set: func(e *Engine, v string) string {
				e.slowlog.slowerThan, _ = strconv.ParseInt(v, 10, 64)
				e.watchLongCommands()
				return ""
			}},
		"slowlog-max-len": {handling: configLive,
			get: func(e *Engine) string { return strconv.Itoa(e.slowlog.maxLen) },
			set: func(e *Engine, v string) string {
				e.slowlog.maxLen, _ = strconv.Atoi(v)
				return ""
			}},
		"slowlog-entry-max-argc": {handling: configLive,
			get: func(e *Engine) string { return strconv.Itoa(e.slowlog.maxArgc) },
			set: func(e *Engine, v string) string {
				e.slowlog.maxArgc, _ = strconv.Atoi(v)
				return ""
			}},
		"slowlog-entry-max-string-len": {handling: configLive,
			get: func(e *Engine) string { return strconv.Itoa(e.slowlog.maxStringLen) },
			set: func(e *Engine, v string) string {
				e.slowlog.maxStringLen, _ = strconv.Atoi(v)
				return ""
			}},
		// Keel's request limits, which it does not change while it runs: a
		// bulk string of at most 512 MiB, Redis's default, and at most 16
		// MiB of a request not yet whole.
		"proto-max-bulk-len":        {handling: configFixed, get: func(*Engine) string { return strconv.Itoa(maxBulkLength) }},
		"client-query-buffer-limit": {handling: configFixed, get: func(*Engine) string { return strconv.Itoa(maxQueryBufferBytes) }},

		// The transport's.
		"port": {handling: configFixed, transport: true, get: func(e *Engine) string { return strconv.Itoa(e.serverInfo.Port) }},
		"bind": {handling: configFixed, transport: true, get: func(e *Engine) string { return e.serverInfo.Host },
			read: func(v string) (string, string) { return v, "" }},
		"tcp-backlog": {handling: configFixed, transport: true, get: func(e *Engine) string { return strconv.Itoa(e.serverInfo.TCPBacklog) }},
		"io-threads":  {handling: configFixed, transport: true, get: func(e *Engine) string { return strconv.Itoa(e.serverInfo.IOThreads) }},
		"maxclients": {handling: configLive, transport: true,
			get: func(e *Engine) string { return strconv.Itoa(e.serverInfo.MaxClients) },
			set: func(e *Engine, v string) string {
				n, _ := strconv.Atoi(v)
				if e.serverInfo.SetMaxClients == nil {
					return errImmutableConfig
				}
				if err := e.serverInfo.SetMaxClients(n); err != "" {
					return err
				}
				e.serverInfo.MaxClients = n
				return ""
			}},
		"hz": {handling: configLive, transport: true,
			get: func(e *Engine) string { return strconv.Itoa(e.serverInfo.Hz) },
			set: func(e *Engine, v string) string {
				// Hz is a hint, Redis says, and a value out of its range is
				// taken at its nearest end (updateHZ).
				n, _ := strconv.Atoi(v)
				n = min(max(n, configMinHz), configMaxHz)
				if e.serverInfo.SetHz == nil {
					return errImmutableConfig
				}
				e.serverInfo.SetHz(n)
				e.serverInfo.Hz = n
				return ""
			}},
		// Redis gives the password to a client that has logged in, which any
		// client asking here has.
		"requirepass": {handling: configLive, transport: true,
			get: func(e *Engine) string { return e.serverInfo.RequirePass },
			set: func(e *Engine, v string) string {
				if e.serverInfo.SetRequirePass == nil {
					return errImmutableConfig
				}
				e.serverInfo.SetRequirePass(v)
				e.serverInfo.RequirePass = v
				return ""
			}},
	}
	for _, name := range storedConfigs {
		m[name] = keelConfig{handling: configStored}
	}
	// The array type's layout (Redis 8.10), checked as Redis checks it.
	m["array-slice-size"] = keelConfig{handling: configStored, valid: validArraySliceSize}
	m["array-sparse-kmax"] = keelConfig{handling: configStored, valid: validArraySparseKmax}
	m["array-sparse-kmin"] = keelConfig{handling: configStored, valid: validArraySparseKmin}
	return m
}

// storedConfigs are the directives that tune only Redis's internals, which
// Keel accepts, keeps and reports without effect (configStored).
var storedConfigs = []string{
	// When a collection leaves its compact encoding, and how it is laid out.
	"hash-max-listpack-entries", "hash-max-listpack-value", "hash-min-template-entries",
	"hash-max-template-entries", "hash-rdb-load-min-template-entries",
	"hash-rdb-load-max-template-entries", "hash-rdb-load-template-disassembly-threshold",
	"list-max-listpack-size", "list-compress-depth", "set-max-intset-entries",
	"set-max-listpack-entries", "set-max-listpack-value", "zset-max-listpack-entries",
	"zset-max-listpack-value", "hll-sparse-max-bytes", "stream-node-max-bytes",
	"stream-node-max-entries",
	// Rehashing, defragmentation, the allocator, lazy freeing and the
	// eviction loop's effort: how Redis spends its CPU and memory.
	"activerehashing", "activedefrag", "active-defrag-cycle-min", "active-defrag-cycle-max",
	"active-defrag-ignore-bytes", "active-defrag-threshold-lower", "active-defrag-threshold-upper",
	"active-defrag-max-scan-fields", "jemalloc-bg-thread", "dynamic-hz", "lazyfree-lazy-eviction",
	"lazyfree-lazy-expire", "lazyfree-lazy-server-del", "lazyfree-lazy-user-del",
	"lazyfree-lazy-user-flush", "lazyexpire-nested-arbitrary-keys", "maxmemory-eviction-tenacity",
	"lookahead", "prefetch-batch-max-size",
	// The format and pace of Redis's own files.
	"rdbcompression", "rdb-save-incremental-fsync", "aof-rewrite-incremental-fsync",
	"rdb-del-sync-files", "aof-use-rdb-preamble", "aof-timestamp-enabled",
	"no-appendfsync-on-rewrite", "sanitize-dump-payload",
	// How Redis moves a snapshot to a replica.
	"repl-diskless-sync", "repl-diskless-sync-delay", "repl-diskless-sync-max-replicas",
	"repl-diskless-load", "repl-rdb-channel", "repl-disable-tcp-nodelay",
	// Cluster settings, which change nothing on a server not in cluster
	// mode, as Keel never is.
	"cluster-allow-pubsubshard-when-down", "cluster-allow-reads-when-down",
	"cluster-allow-replica-migration", "cluster-link-sendbuf-limit", "cluster-migration-barrier",
	"cluster-node-timeout", "cluster-ping-interval", "cluster-replica-no-failover",
	"cluster-replica-validity-factor", "cluster-require-full-coverage", "cluster-slot-stats-enabled",
	"cluster-compatibility-sample-ratio", "cluster-slot-migration-handoff-max-lag-bytes",
	"cluster-slot-migration-max-archived-tasks", "cluster-slot-migration-sync-buffer-drain-timeout",
	"cluster-slot-migration-write-pause-timeout", "cluster-preferred-endpoint-type",
	// Redis's crash reports, its log's level and what it hides, and the
	// delays and watchdog its own tests set.
	"crash-log-enabled", "crash-memcheck-enabled", "use-exit-on-panic", "hide-user-data-from-log",
	"loglevel", "watchdog-period", "rdb-key-save-delay", "key-load-delay",
	"loading-process-events-interval-bytes", "aof-disable-auto-gc",
	// The longest a script runs before Redis answers others BUSY: Keel runs
	// no scripts.
	"busy-reply-threshold",
}

// The bounds Redis holds hz to (CONFIG_MIN_HZ, CONFIG_MAX_HZ).
const (
	configMinHz = 1
	configMaxHz = 500
)

// maxQueryBufferBytes is the most of a request not yet whole the transport
// holds for one connection (internal/server's maxQueryBuffer).
const maxQueryBufferBytes = 16 * 1024 * 1024

// setOption changes one of e's options as CONFIG SET does, and returns the
// error text if the engine refuses the result (which a value Redis's checks
// accept should never be).
func (e *Engine) setOption(change func(*Options)) string {
	o := e.options
	change(&o)
	if err := o.validate(); err != nil {
		return err.Error()
	}
	e.options = o
	e.space.SetLimits(o.limits())
	e.settings = o.settings()
	return ""
}

// zeroIsOff is Redis's zero, which turns a setting off, as an Options field
// reads it: Off, since zero there is the default.
func zeroIsOff[T int | int64](v T) T {
	if v == 0 {
		return Off
	}
	return v
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// readSave reads save as setConfigSaveOption does: pairs of seconds, at least
// 1, and changes, at least 0, or nothing at all.
func readSave(v string) (string, string) {
	if v == "" {
		return "", ""
	}
	args := strings.Split(v, " ")
	if len(args)%2 != 0 {
		return "", "Invalid save parameters"
	}
	for i, arg := range args {
		n, ok := cLong(arg)
		if !ok || (i%2 == 0 && n < 1) || (i%2 == 1 && n < 0) {
			return "", "Invalid save parameters"
		}
	}
	return v, ""
}

// cLong is C's strtoll as Redis checks it (the whole string read): optional
// leading white space and sign, then digits. An overflow reads as the
// nearest end, as strtoll's ERANGE does.
func cLong(s string) (int64, bool) {
	t := strings.TrimLeft(s, " \t\n\v\f\r")
	neg := false
	if t != "" && (t[0] == '+' || t[0] == '-') {
		neg = t[0] == '-'
		t = t[1:]
	}
	if t == "" {
		// strtoll reads nothing, and the end pointer is the string's start.
		return 0, s == ""
	}
	var v uint64
	for i := 0; i < len(t); i++ {
		if t[i] < '0' || t[i] > '9' {
			return 0, false
		}
		if v < 1<<63 {
			v = v*10 + uint64(t[i]-'0')
		}
	}
	if neg {
		if v > 1<<63 {
			v = 1 << 63
		}
		return -int64(v), true
	}
	if v > 1<<63-1 {
		v = 1<<63 - 1
	}
	return int64(v), true
}

// readPercentiles reads latency-tracking-info-percentiles as Redis's
// setConfigLatencyTrackingInfoPercentilesOutputOption does - numbers from 0 to
// 100, as string2d reads them, or none at all - and writes them back as its
// getter does, each with %f and its trailing zeros trimmed.
func readPercentiles(v string) (string, string) {
	if v == "" {
		return "", ""
	}
	args := strings.Split(v, " ")
	out := make([]string, len(args))
	for i, arg := range args {
		p, err := strconv.ParseFloat(arg, 64)
		if err != nil || math.IsNaN(p) {
			return "", "Invalid latency-tracking-info-percentiles parameters"
		}
		if p > 100 || p < 0 {
			return "", "latency-tracking-info-percentiles parameters should sit between [0.0,100.0]"
		}
		s := strconv.FormatFloat(p, 'f', 6, 64)
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
		out[i] = s
	}
	return strings.Join(out, " "), ""
}

// keyspaceEventClasses are the characters notify-keyspace-events takes in a
// build of Redis 8.10.1 without GCRA (notify.c, keyspaceEventsStringToFlags).
const keyspaceEventClasses = "Ag$lshzxeKEtmdnocaSTIV"

// readKeyspaceEvents reads notify-keyspace-events as Redis does, and writes
// it back as Redis would: the classes it turns on, none being "".
func readKeyspaceEvents(v string) (string, string) {
	for i := 0; i < len(v); i++ {
		if !strings.ContainsRune(keyspaceEventClasses, rune(v[i])) {
			return "", "Invalid event class character. Use 'Ag$lshzxeKEtmdnocaSTIV'."
		}
	}
	if v == "" {
		return "", ""
	}
	// Any class turned on is a value Keel cannot have; what it is matters
	// only in that it is not "".
	return v, ""
}

func validArraySliceSize(_ func(string) string, v string) string {
	n, _ := strconv.ParseInt(v, 10, 64)
	if n <= 0 || n&(n-1) != 0 {
		return "array-slice-size must be a power of two"
	}
	return ""
}

func validArraySparseKmax(setting func(string) string, v string) string {
	n, _ := strconv.ParseInt(v, 10, 64)
	kmin, _ := strconv.ParseInt(setting("array-sparse-kmin"), 10, 64)
	if n > 0 && uint32(n) <= uint32(kmin) {
		return "array-sparse-kmax must be greater than array-sparse-kmin when non-zero"
	}
	return ""
}

func validArraySparseKmin(setting func(string) string, v string) string {
	n, _ := strconv.ParseInt(v, 10, 64)
	kmax, _ := strconv.ParseInt(setting("array-sparse-kmax"), 10, 64)
	if kmax > 0 && uint32(n) >= uint32(kmax) {
		return "array-sparse-kmin must be less than array-sparse-kmax"
	}
	return ""
}
