package core

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/brandopakel/keel/internal/config"
	"github.com/brandopakel/keel/internal/data_structure"
)

// The log's bytes, held to what develop wrote before step 2.3 of the embedding
// plan (docs/embedding-plan.md) moved persistence into the engine.
//
// Every scenario below runs a fixed sequence of commands with the log open and
// keeps three things: the log it leaves, the replies it got, and the keyspace
// it ends with. testdata/persistence-40eb2f6/ holds what develop at 40eb2f6
// produced, written by these tests with
//
//	KEEL_CAPTURE_PERSISTENCE_GOLDEN=$PWD/testdata/persistence-40eb2f6 \
//	  go test ./internal/core -run '^TestPersistenceGoldenCapture$'
//
// and every run since compares with it, under every fsync policy, with
// synchronous and worker appends:
//
//   - the log must be the same bytes, record for record;
//   - the replies must be the same bytes;
//   - the keyspace must be the same, and replaying develop's log (the fixture)
//     must give the same keyspace again, which is the upgrade direction. The
//     rollback direction, develop's build replaying this build's log, follows
//     from the logs being the same bytes; the log-compatibility job builds both
//     and checks it with the binaries.
//
// Two things in a log are not the same from run to run, and are normalized on
// both sides before the comparison, never anywhere else:
//
//   - A relative expiry is logged as the instant it falls due, which depends on
//     when the command ran. Every relative TTL here is a whole number of hours,
//     and an instant within a day of the run is written as goldenBase plus the
//     whole hours from the run's start. Absolute expiries are after 2100 and
//     are left alone.
//   - A hash, and a sorted set small enough to be one record, are rewritten
//     from a Go map, whose order is random. So the field-value pairs of each
//     HSET record are put in field order, and the score-member pairs of each
//     ZADD record without options in member order. The sort is stable, so a
//     field or member named twice keeps its last value.
//
// A large hash is rewritten in several HSET records whose split is random too,
// so the scenarios hold small hashes only; aof_rewrite_test.go and
// rewrite_collections_test.go cover the large ones. A large sorted set is
// rewritten in rank order, which is not random. Map order also makes the
// rewrite's dirty keys come out in random order, so each rewrite here has
// exactly one key written while it runs.
const persistenceGoldenDir = "testdata/persistence-40eb2f6"

// goldenBase is year 2096, after anything the scenarios write relative to now
// and before their absolute expiries.
const goldenBase = 4_000_000_000_000

// goldenAbsolute is 2100-01-01T00:00:00Z, in seconds.
const goldenAbsolute = "4102444800"

type goldenMode struct {
	name   string
	fsync  string
	worker bool
}

// goldenModes are the policies and append modes a server can run with. The
// log's bytes must not depend on them, so each scenario is held to the same
// fixture under all of them.
var goldenModes = []goldenMode{
	{"everysec", config.FsyncEverySec, false},
	{"always", config.FsyncAlways, false},
	{"no", config.FsyncNever, false},
	{"worker-always", config.FsyncAlways, true},
	{"worker-everysec", config.FsyncEverySec, true},
}

type goldenScenario struct {
	name string
	// seed, when set, is a log written before the server starts, which it
	// replays and then appends to.
	seed func() []byte
	run  func(r *goldenRun)
}

var goldenScenarios = []goldenScenario{
	{name: "session", run: goldenSession},
	{name: "evictions", run: goldenEvictions},
	{name: "rewrite-dirty-before-walk", run: goldenRewriteDirtyBeforeWalk},
	{name: "rewrite-dirty-during-preflush", run: goldenRewriteDirtyDuringPreflush},
	{name: "rewrite-scheduled-by-exec", run: goldenRewriteScheduledByExec},
	{name: "legacy-filters", seed: goldenLegacyFilterLog, run: goldenLegacyFilters},
}

// goldenRun is one scenario under one mode: the log is open at path, and
// every command is followed by the flush the event loop runs after a cycle.
type goldenRun struct {
	t       *testing.T
	mode    goldenMode
	path    string
	start   int64
	tx      *Transaction
	replies bytes.Buffer
}

// startGoldenRun opens a run, and returns it with the function that ends it:
// the log closed, the keyspace emptied and the settings it changed restored.
func startGoldenRun(t *testing.T, mode goldenMode, seed []byte) (*goldenRun, func()) {
	t.Helper()
	fsync, async, percentage := config.AOFFsync, config.AOFAsyncAppend, config.AOFAutoRewritePercentage
	maxMemory, strategy, samples := config.MaxMemory, config.EvictStrategy, config.LRUSamples
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		CancelRewrite()
		CloseAOF()
		config.AOFFsync, config.AOFAsyncAppend, config.AOFAutoRewritePercentage = fsync, async, percentage
		config.MaxMemory, config.EvictStrategy, config.LRUSamples = maxMemory, strategy, samples
		ResetStores()
	}
	t.Cleanup(stop)
	config.AOFFsync, config.AOFAsyncAppend = mode.fsync, mode.worker
	// Automatic rewrites start on growth, which a scenario starts itself.
	config.AOFAutoRewritePercentage = 0
	ResetStores()
	r := &goldenRun{t: t, mode: mode, path: filepath.Join(t.TempDir(), "golden.aof")}
	if seed != nil {
		require.NoError(t, os.WriteFile(r.path, seed, 0o644))
		_, err := LoadAOF(r.path)
		require.NoError(t, err)
	}
	require.NoError(t, OpenAOF(r.path))
	r.start = time.Now().UnixMilli()
	return r, stop
}

// cycle flushes as the event loop does once a run of commands has executed:
// FlushAOF, or with worker appends FlushAOFAsync until the batch is written,
// which is the barrier a server without concurrent appends waits at.
func (r *goldenRun) cycle() {
	r.t.Helper()
	if !r.mode.worker {
		require.NoError(r.t, FlushAOF())
		return
	}
	for i := 0; ; i++ {
		ready, err := FlushAOFAsync(nil)
		require.NoError(r.t, err)
		if ready {
			return
		}
		require.Less(r.t, i, 200000, "the append worker did not finish")
		time.Sleep(25 * time.Microsecond)
	}
}

// do runs one command as a connection sends it, inside the open transaction
// if there is one, then flushes, and keeps its reply.
func (r *goldenRun) do(parts ...string) string {
	r.t.Helper()
	cmd := &Command{Cmd: strings.ToUpper(parts[0]), Args: parts[1:]}
	var w replyWriter
	var err error
	if r.tx != nil || IsTransactionCommand(cmd.Cmd) {
		r.tx, err = Transact(r.tx, cmd, &w, nil)
	} else {
		err = EvalAndResponse(cmd, &w)
	}
	require.NoError(r.t, err, "%.80q", parts)
	r.cycle()
	r.replies.Write(w.b)
	return string(w.b)
}

// ok runs a command that must succeed.
func (r *goldenRun) ok(parts ...string) {
	r.t.Helper()
	reply := r.do(parts...)
	require.False(r.t, strings.HasPrefix(reply, "-"), "%.80q answered %q", parts, reply)
}

// unrecorded runs a read whose reply depends on the clock or chance, so it is
// left out of the replies compared.
func (r *goldenRun) unrecorded(parts ...string) {
	r.t.Helper()
	n := r.replies.Len()
	r.do(parts...)
	r.replies.Truncate(n)
}

// driveRewrite runs the event loop's part of a rewrite, a cycle at a time,
// until it ends or until stop says to.
func (r *goldenRun) driveRewrite(stop func() bool) {
	r.t.Helper()
	for i := 0; RewriteActive(); i++ {
		if stop != nil && stop() {
			return
		}
		r.cycle()
		require.Less(r.t, i, 200000, "the rewrite did not end")
		time.Sleep(20 * time.Microsecond)
	}
}

// goldenLog is a scenario's result: its log and keyspace after normalizing,
// and its replies.
type goldenLog struct {
	log, replies, state []byte
}

func (r *goldenRun) finish() goldenLog {
	r.t.Helper()
	require.Nil(r.t, r.tx, "a transaction was left open")
	require.False(r.t, RewriteActive())
	r.cycle()
	raw, err := os.ReadFile(r.path)
	require.NoError(r.t, err)
	window := goldenWindow{r.start, time.Now().UnixMilli()}
	return goldenLog{log: normalizeGoldenLog(r.t, raw, window), replies: r.replies.Bytes(), state: goldenState(r.t, window)}
}

func runGoldenScenario(t *testing.T, scenario goldenScenario, mode goldenMode) goldenLog {
	t.Helper()
	var seed []byte
	if scenario.seed != nil {
		seed = scenario.seed()
	}
	r, stop := startGoldenRun(t, mode, seed)
	defer stop()
	scenario.run(r)
	return r.finish()
}

// goldenWindow is when a scenario ran, in Unix milliseconds.
type goldenWindow struct{ start, end int64 }

// instant normalizes an expiry: one set relative to the run, within a day of
// it, becomes goldenBase plus the whole hours from the run's start.
func (w goldenWindow) instant(at uint64) uint64 {
	if int64(at) >= w.start && int64(at) <= w.end+int64(24*time.Hour/time.Millisecond) {
		hours := (int64(at) - w.start) / int64(time.Hour/time.Millisecond)
		return goldenBase + uint64(hours)*uint64(time.Hour/time.Millisecond)
	}
	return at
}

// goldenRecords splits a log into its records, each as the strings it holds.
func goldenRecords(t *testing.T, log []byte) [][]string {
	t.Helper()
	var records [][]string
	line := func() int {
		end := bytes.Index(log, []byte("\r\n"))
		require.Positive(t, end, "a record header")
		n, err := strconv.Atoi(string(log[1:end]))
		require.NoError(t, err)
		log = log[end+2:]
		return n
	}
	for len(log) > 0 {
		require.Equal(t, byte('*'), log[0])
		parts := make([]string, line())
		for i := range parts {
			require.Equal(t, byte('$'), log[0])
			n := line()
			require.GreaterOrEqual(t, len(log), n+2)
			parts[i] = string(log[:n])
			require.Equal(t, "\r\n", string(log[n:n+2]))
			log = log[n+2:]
		}
		records = append(records, parts)
	}
	return records
}

// normalizeGoldenLog applies the two normalizations described at the top of
// this file and nothing else, record by record.
func normalizeGoldenLog(t *testing.T, log []byte, window goldenWindow) []byte {
	t.Helper()
	var out []byte
	for _, parts := range goldenRecords(t, log) {
		switch {
		case strings.EqualFold(parts[0], "PEXPIREAT") && len(parts) == 3:
			at, err := strconv.ParseUint(parts[2], 10, 64)
			require.NoError(t, err)
			parts[2] = strconv.FormatUint(window.instant(at), 10)
		case strings.EqualFold(parts[0], "HSET") && len(parts)%2 == 0:
			sortGoldenPairs(parts[2:], 0)
		case strings.EqualFold(parts[0], "ZADD") && len(parts)%2 == 0 && goldenScores(parts[2:]):
			sortGoldenPairs(parts[2:], 1)
		}
		out = appendCommand(out, parts...)
	}
	return out
}

// sortGoldenPairs puts the pairs in parts in the order of the element at by
// in each, keeping pairs that tie in their order.
func sortGoldenPairs(parts []string, by int) {
	pairs := make([][2]string, 0, len(parts)/2)
	for i := 0; i < len(parts); i += 2 {
		pairs = append(pairs, [2]string{parts[i], parts[i+1]})
	}
	slices.SortStableFunc(pairs, func(a, b [2]string) int { return strings.Compare(a[by], b[by]) })
	for i, p := range pairs {
		parts[2*i], parts[2*i+1] = p[0], p[1]
	}
}

// goldenScores reports whether every other part, from the first, is a score:
// a ZADD with no options.
func goldenScores(parts []string) bool {
	for i := 0; i < len(parts); i += 2 {
		if _, err := strconv.ParseFloat(parts[i], 64); err != nil {
			return false
		}
	}
	return true
}

// goldenValue writes a value into the state text, or its digest if it is long.
func goldenValue(b *bytes.Buffer, value string) {
	if len(value) <= 64 {
		fmt.Fprintf(b, " %q", value)
		return
	}
	sum := sha256.Sum256([]byte(value))
	fmt.Fprintf(b, " sha256:%s/%d", hex.EncodeToString(sum[:8]), len(value))
}

// goldenState describes the default engine's keyspace; see engineState.
func goldenState(t *testing.T, window goldenWindow) []byte {
	t.Helper()
	return engineState(t, defaultEngine, window)
}

// engineState describes e's keyspace, one key a line in key order: the store
// that holds it, its expiry and its value. It reads without touching, so it
// neither reaps nor counts as a use.
func engineState(t *testing.T, e *Engine, window goldenWindow) []byte {
	t.Helper()
	type held struct {
		key string
		ks  data_structure.Keyspace
	}
	var keys []held
	e.space.EachKeyspace(func(ks data_structure.Keyspace) {
		for _, key := range ks.Keys() {
			keys = append(keys, held{key, ks})
		}
	})
	slices.SortFunc(keys, func(a, b held) int { return strings.Compare(a.key, b.key) })
	var b bytes.Buffer
	for _, k := range keys {
		var line bytes.Buffer
		fmt.Fprintf(&line, "%s %q", k.ks.KeyspaceName(), k.key)
		if at, ok := k.ks.GetExpiry(k.key); ok {
			fmt.Fprintf(&line, " expires:%d", window.instant(at))
		}
		present := true
		if obj := e.dictStore.Peek(k.key); obj != nil {
			goldenValue(&line, obj.Value)
		} else if h, ok := e.hashStore.Peek(k.key); ok {
			fields, values := h.Entries()
			order := make([]int, len(fields))
			for i := range order {
				order[i] = i
			}
			slices.SortFunc(order, func(i, j int) int { return strings.Compare(fields[i], fields[j]) })
			for _, i := range order {
				goldenValue(&line, fields[i])
				goldenValue(&line, values[i])
			}
		} else if l, ok := e.listStore.Peek(k.key); ok {
			for _, v := range l.All() {
				goldenValue(&line, v)
			}
		} else if s, ok := e.setStore.Peek(k.key); ok {
			members := s.Members()
			slices.Sort(members)
			for _, m := range members {
				goldenValue(&line, m)
			}
		} else if z, ok := e.zsetStore.Peek(k.key); ok {
			members, scores := z.Entries()
			order := make([]int, len(members))
			for i := range order {
				order[i] = i
			}
			slices.SortFunc(order, func(i, j int) int { return strings.Compare(members[i], members[j]) })
			for _, i := range order {
				goldenValue(&line, members[i])
				fmt.Fprintf(&line, " %s", formatScore(scores[i]))
			}
		} else if plan, ok := e.planDump(k.key, math.MaxInt-9); ok {
			goldenValue(&line, string(appendDump(nil, plan)))
		} else {
			present = false // expired, and not yet reaped
		}
		if present {
			b.Write(line.Bytes())
			b.WriteByte('\n')
		}
	}
	return b.Bytes()
}

// hours is a relative TTL of n hours, in the unit given.
func hours(n int, unit time.Duration) string {
	return strconv.FormatInt(int64(time.Duration(n)*time.Hour/unit), 10)
}

// goldenSession is every command family, its writes, reads and refusals,
// the records logged in place of a command, expiry of each kind, SET over
// another type, and transactions.
func goldenSession(r *goldenRun) {
	sec, ms := time.Second, time.Millisecond

	// FLUSHDB, inside a transaction, between keys written before and after.
	r.ok("SET", "f:1", "v")
	r.ok("MULTI")
	r.ok("SET", "f:2", "v")
	r.ok("FLUSHDB")
	r.ok("SET", "f:3", "v")
	r.ok("EXEC")
	r.ok("SET", "f:4", "v")

	// Strings.
	r.ok("SET", "s:plain", "v1")
	r.ok("SET", "s:plain", "v2")
	r.ok("SETNX", "s:nx", "a")
	r.ok("SETNX", "s:nx", "b")
	r.ok("SET", "s:nx2", "a", "NX")
	r.ok("SET", "s:nx2", "b", "NX")
	r.ok("SET", "s:missing", "a", "XX")
	r.ok("SET", "s:plain", "v3", "XX")
	r.ok("SET", "s:plain", "v4", "GET")
	r.ok("MSET", "m:1", "a", "m:2", "b", "m:3", "c")
	r.ok("INCR", "c")
	r.ok("INCRBY", "c", "41")
	r.ok("DECR", "c")
	r.ok("DECRBY", "c", "2")
	r.ok("SET", "c:text", "abc")
	r.do("INCR", "c:text")
	r.do("INCRBY", "c", "nope")
	r.ok("SET", "s:ex", "v", "EX", hours(1, sec))
	r.ok("SET", "s:px", "v", "PX", hours(2, ms))
	r.ok("SET", "s:exat", "v", "EXAT", goldenAbsolute)
	r.ok("SET", "s:pxat", "v", "PXAT", goldenAbsolute+"123")
	r.ok("SETEX", "s:setex", hours(3, sec), "v")
	r.ok("PSETEX", "s:psetex", hours(4, ms), "v")
	r.ok("SET", "s:ex", "kept", "KEEPTTL")
	r.do("SET", "s:bad", "v", "EX", "0")
	r.ok("SET", "s:huge", strings.Repeat("0123456789abcdef", 5<<16)) // 5 MiB: drains in fragments
	r.ok("GET", "s:plain")
	r.ok("MGET", "s:plain", "missing", "m:1")
	r.ok("LCS", "m:1", "m:2")
	r.ok("GET", "s:huge")

	// Keys and expiry.
	r.ok("EXPIRE", "s:plain", hours(5, sec))
	r.ok("PEXPIRE", "m:1", hours(6, ms))
	r.ok("EXPIREAT", "m:2", goldenAbsolute)
	r.ok("PEXPIREAT", "c", goldenAbsolute+"999")
	r.ok("PERSIST", "m:2")
	r.ok("EXPIRE", "missing", "10")
	r.ok("EXPIRE", "s:plain", hours(7, sec), "XX")
	r.ok("EXPIRE", "s:plain", hours(1, sec), "GT")
	r.ok("PEXPIREAT", "s:nx", "1000") // in the past: a DEL
	r.ok("EXPIRE", "s:nx2", "-1")
	r.ok("DEL", "s:setex", "missing")
	r.ok("UNLINK", "s:psetex")
	r.ok("EXISTS", "s:plain", "s:setex")
	r.ok("TYPE", "s:plain")
	r.unrecorded("TTL", "s:plain")
	r.unrecorded("PTTL", "s:px")
	r.ok("KEYS", "m:*")
	r.ok("SCAN", "0", "MATCH", "s:*", "COUNT", "100")
	r.ok("DBSIZE")
	// Lazy expiry: a read reaps the key and logs its DEL.
	r.ok("SET", "s:brief", "v", "PX", "1")
	time.Sleep(5 * time.Millisecond)
	r.ok("GET", "s:brief")
	// Active expiry: the cycle reaps the key and logs its DEL.
	r.ok("SET", "s:brief2", "v", "PX", "1")
	time.Sleep(5 * time.Millisecond)
	for i := 0; ExpireCycle() == 0; i++ {
		require.Less(r.t, i, 1000, "the expiry cycle did not reap s:brief2")
	}
	r.cycle()

	// Hashes.
	r.ok("HSET", "h", "f1", "v1", "f2", "v2", "f3", "v3")
	r.ok("HSETNX", "h", "f1", "x")
	r.ok("HSETNX", "h", "f4", "v4")
	r.ok("HDEL", "h", "f2", "missing")
	r.ok("HINCRBY", "h", "n", "5")
	r.do("HINCRBY", "h", "f1", "1")
	r.ok("HGET", "h", "f1")
	r.ok("HMGET", "h", "f1", "missing", "n")
	r.ok("HLEN", "h")
	r.ok("HEXISTS", "h", "f3")
	r.ok("HSET", "h:one", "f", "v")
	r.ok("HGETALL", "h:one")
	r.ok("HKEYS", "h:one")
	r.ok("HVALS", "h:one")

	// Lists.
	r.ok("RPUSH", "l", "a", "b", "c", "d")
	r.ok("LPUSH", "l", "z", "y")
	r.ok("LPOP", "l")
	r.ok("RPOP", "l")
	r.ok("LPOP", "l", "1")
	r.ok("LSET", "l", "0", "q")
	r.ok("LTRIM", "l", "0", "1")
	r.ok("LRANGE", "l", "0", "-1")
	r.ok("LINDEX", "l", "1")
	r.ok("LLEN", "l")
	r.ok("RPUSH", "l:empty", "x")
	r.ok("RPOP", "l:empty")
	r.do("LSET", "l", "9", "x")

	// Sets.
	r.ok("SADD", "set", "a", "b", "c", "d")
	r.ok("SREM", "set", "d", "missing")
	r.ok("SADD", "set:one", "only")
	r.ok("SPOP", "set:one") // logged as the SREM it was
	r.ok("SADD", "set:two", "only")
	r.ok("SPOP", "set:two", "1")
	r.ok("SMEMBERS", "set")
	r.ok("SISMEMBER", "set", "a")
	r.ok("SMISMEMBER", "set", "a", "z")
	r.ok("SCARD", "set")
	r.unrecorded("SRANDMEMBER", "set")

	// Sorted sets and geo.
	r.ok("ZADD", "z", "1", "a", "2", "b", "3", "c")
	r.ok("ZADD", "z", "NX", "5", "a", "4", "d")
	r.ok("ZADD", "z", "XX", "CH", "10", "b")
	r.do("ZADD", "z", "GT", "1", "c")
	r.ok("ZINCRBY", "z", "2.5", "a")
	r.ok("ZREM", "z", "c", "missing")
	r.ok("ZPOPMIN", "z")
	r.ok("ZADD", "z2", "1", "x", "2", "y", "3", "w")
	r.ok("ZPOPMAX", "z2", "2")
	r.ok("ZRANGE", "z", "0", "-1", "WITHSCORES")
	r.ok("ZRANGEBYSCORE", "z", "-inf", "+inf")
	r.ok("ZREVRANGEBYSCORE", "z", "+inf", "-inf")
	r.ok("ZCOUNT", "z", "0", "100")
	r.ok("ZRANK", "z", "a")
	r.ok("ZSCORE", "z", "a")
	r.ok("ZCARD", "z")
	r.ok("GEOADD", "geo", "13.361389", "38.115556", "palermo", "15.087269", "37.502669", "catania")
	r.ok("GEODIST", "geo", "palermo", "catania", "km")
	r.ok("GEOPOS", "geo", "palermo")
	r.ok("GEOHASH", "geo", "palermo", "catania")
	r.ok("GEOSEARCH", "geo", "FROMLONLAT", "15", "37", "BYRADIUS", "200", "km", "ASC")

	// Filters and sketches.
	r.ok("BF.RESERVE", "bf", "0.01", "1000")
	r.ok("BF.ADD", "bf", "a")
	r.ok("BF.MADD", "bf", "b", "c", "a")
	r.ok("BF.ADD", "bf:auto", "x")
	r.ok("BF.EXISTS", "bf", "a")
	r.ok("BF.MEXISTS", "bf", "a", "q")
	r.ok("CF.RESERVE", "cf", "1000")
	r.ok("CF.ADD", "cf", "a")
	r.ok("CF.ADDNX", "cf", "a")
	r.ok("CF.ADD", "cf", "b")
	r.ok("CF.DEL", "cf", "a")
	r.ok("CF.ADD", "cf:auto", "x")
	r.ok("CF.EXISTS", "cf", "b")
	r.ok("CF.COUNT", "cf", "b")
	r.ok("CMS.INITBYDIM", "cms", "100", "5")
	r.ok("CMS.INCRBY", "cms", "a", "3", "b", "4")
	r.ok("CMS.INITBYPROB", "cms:prob", "0.01", "0.01")
	r.ok("CMS.QUERY", "cms", "a", "b")
	r.ok("MORRIS.INITBYDIM", "mor", "200", "5")
	r.ok("MORRIS.INCRBY", "mor", "hits", "500")
	r.ok("MORRIS.INITBYPROB", "mor:prob", "0.01", "0.01")
	r.ok("MORRIS.QUERY", "mor", "hits")
	r.ok("PFADD", "hll", "a", "b", "c")
	r.ok("PFADD", "hll2", "c", "d")
	r.ok("PFMERGE", "hll3", "hll", "hll2")
	r.ok("PFCOUNT", "hll3")

	// Dump and restore, under the current name and the one before the rename.
	payload := r.do("KEEL.DUMP", "l")
	image, _ := Decode([]byte(payload))
	r.ok("KEEL.RESTORE", "l:copy", image.(string))
	payload = r.do("KEEL.DUMP", "bf")
	image, _ = Decode([]byte(payload))
	r.ok("MEMKV.RESTORE", "bf:copy", image.(string))

	// SET over another type is logged as DEL, then SET.
	r.ok("HSET", "x:hash", "f", "v")
	r.ok("SET", "x:hash", "string now")
	r.ok("SADD", "x:set", "m")
	r.do("SET", "x:set", "v", "GET")
	r.ok("RPUSH", "x:list", "a")
	r.ok("SET", "x:list", "v", "NX")
	r.ok("ZADD", "x:z", "1", "m")
	r.ok("SET", "x:z", "v", "XX")
	r.ok("BF.ADD", "x:bf", "a")
	r.ok("PEXPIREAT", "x:bf", goldenAbsolute+"456")
	r.ok("SET", "x:bf", "v", "KEEPTTL")
	r.do("LPUSH", "s:plain", "x")
	r.do("HSET", "set", "f", "v")

	// Transactions: a block of writes is framed; a block of reads, and
	// discarded or aborted ones, write nothing; a command that fails inside
	// EXEC is not logged, and the rest are; a key reaped inside the block is
	// deleted inside it; a relative expiry and an SPOP inside it are logged
	// as their canonical records.
	r.ok("MULTI")
	r.ok("SET", "t:1", "a")
	r.ok("INCR", "t:2")
	r.ok("HSET", "t:3", "f", "v")
	r.ok("EXEC")
	r.ok("MULTI")
	r.ok("GET", "t:1")
	r.ok("HGET", "t:3", "f")
	r.ok("EXEC")
	r.ok("MULTI")
	r.ok("SET", "t:4", "a")
	r.ok("LPUSH", "t:4", "x")
	r.ok("INCR", "t:5")
	r.ok("EXEC")
	r.ok("SET", "t:brief", "v", "PX", "1")
	time.Sleep(5 * time.Millisecond)
	r.ok("MULTI")
	r.ok("GET", "t:brief")
	r.ok("SET", "t:6", "v")
	r.ok("EXEC")
	r.ok("MULTI")
	r.do("SET", "t:7")
	r.do("EXEC")
	r.ok("MULTI")
	r.ok("SET", "t:8", "v")
	r.ok("DISCARD")
	r.ok("SADD", "t:pop", "only")
	r.ok("MULTI")
	r.ok("EXPIRE", "t:1", hours(8, sec))
	r.ok("SET", "t:9", "v", "EX", hours(9, sec))
	r.ok("SPOP", "t:pop")
	r.ok("ZADD", "t:z", "1", "a")
	r.ok("EXEC")
}

// goldenEvictions writes past a memory budget, so the key written is evicted
// at once and its DEL follows its record - after the EXEC of a transaction.
// Each write is the only key, so the eviction has one choice.
func goldenEvictions(r *goldenRun) {
	config.MaxMemory, config.EvictStrategy, config.LRUSamples = 1, config.LRU, 16
	r.ok("SET", "e:1", "v")
	r.ok("MULTI")
	r.ok("SET", "e:2", "v")
	r.ok("EXEC")
	r.ok("HSET", "e:3", "f", "v")
	r.ok("DBSIZE")
}

// goldenPopulate fills a keyspace a rewrite has to work for: a long list and
// large set and sorted set, which it writes over several records; a filter
// and a sketch larger than one record slice, which it streams; and small keys
// of every type, with expiries.
func goldenPopulate(r *goldenRun) {
	for i := range 40 {
		r.ok("SET", "s:"+strconv.Itoa(i), "value:"+strconv.Itoa(i))
	}
	r.ok("SET", "s:abs", "v", "PXAT", goldenAbsolute+"123")
	r.ok("SET", "s:rel", "v", "EX", hours(2, time.Second))
	r.ok("HSET", "h", "f1", "v1", "f2", "v2", "f3", "v3", "f4", "v4")
	r.ok("HSET", "h:ttl", "f1", "v1", "f2", "v2")
	r.ok("PEXPIREAT", "h:ttl", goldenAbsolute+"321")
	list := []string{"RPUSH", "big:list"}
	for i := range 40 {
		list = append(list, strings.Repeat(string(rune('a'+i%26)), 4096)+strconv.Itoa(i))
	}
	r.ok(list...)
	set := []string{"SADD", "big:set"}
	zset := []string{"ZADD", "big:z"}
	for i := range 300 {
		set = append(set, "m:"+strconv.Itoa(i))
		zset = append(zset, strconv.Itoa(i*3), "m:"+strconv.Itoa(i))
	}
	r.ok(set...)
	r.ok(zset...)
	r.ok("PEXPIREAT", "big:z", goldenAbsolute+"789")
	r.ok("RPUSH", "l", "a", "b", "c")
	r.ok("SADD", "set", "a", "b", "c")
	r.ok("ZADD", "z", "1", "a", "2.5", "b")
	r.ok("GEOADD", "geo", "13.361389", "38.115556", "palermo")
	r.ok("BF.RESERVE", "big:bf", "0.001", "100000")
	r.ok("BF.MADD", "big:bf", "a", "b", "c")
	r.ok("PEXPIRE", "big:bf", hours(3, time.Millisecond))
	r.ok("CMS.INITBYDIM", "big:cms", "10000", "4")
	r.ok("CMS.INCRBY", "big:cms", "a", "3")
	r.ok("MORRIS.INITBYDIM", "mor", "200", "5")
	r.ok("MORRIS.INCRBY", "mor", "hits", "500")
	r.ok("PFADD", "hll", "a", "b", "c", "d")
	r.ok("CF.RESERVE", "cf", "1000")
	r.ok("CF.ADD", "cf", "a")
}

func requireRewriteSucceeded(r *goldenRun) {
	r.t.Helper()
	require.Equal(r.t, "ok", persistenceField(r.t, "aof_last_bgrewrite_status"))
	require.Equal(r.t, "1", persistenceField(r.t, "aof_rewrites"))
}

// goldenRewriteDirtyBeforeWalk: a key is replaced by another type after the
// rewrite starts and before its walk begins, so the walk passes it by and
// its reconciliation writes it, with a DEL first. Writes after the swap go to
// the new log.
func goldenRewriteDirtyBeforeWalk(r *goldenRun) {
	goldenPopulate(r)
	require.NoError(r.t, StartRewrite())
	r.ok("SET", "h", "replaced")
	r.driveRewrite(nil)
	requireRewriteSucceeded(r)
	r.ok("SET", "after", "v")
	r.ok("RPUSH", "big:list", "after")
}

// goldenRewriteDirtyDuringPreflush: a key the walk has already written is
// written to again while the snapshot's preflush sync is pending, so the
// dirty tail writes the whole key again after it, with a DEL first.
func goldenRewriteDirtyDuringPreflush(r *goldenRun) {
	goldenPopulate(r)
	require.NoError(r.t, StartRewrite())
	r.driveRewrite(func() bool { return persistenceField(r.t, "aof_rewrite_pending_sync") == "1" })
	require.True(r.t, RewriteActive(), "the preflush sync started")
	r.ok("RPUSH", "big:list", "during")
	r.driveRewrite(nil)
	requireRewriteSucceeded(r)
	r.ok("SET", "after", "v")
}

// goldenRewriteScheduledByExec: a BGREWRITEAOF inside a transaction is
// scheduled, and starts once the block has reached the old log.
func goldenRewriteScheduledByExec(r *goldenRun) {
	goldenPopulate(r)
	r.ok("MULTI")
	r.ok("SET", "sched", "v")
	r.ok("BGREWRITEAOF")
	r.ok("EXEC")
	require.True(r.t, RewriteActive(), "the scheduled rewrite started after EXEC")
	r.driveRewrite(nil)
	requireRewriteSucceeded(r)
	r.ok("SET", "after", "v")
}

// goldenLegacyFilterLog is the log of filters the build before RedisBloom
// parity wrote, which replays as that build read it (#94).
func goldenLegacyFilterLog() []byte {
	var body []byte
	for _, args := range legacyBloomLog {
		body = appendCommand(body, args...)
	}
	return body
}

// goldenLegacyFilters replays the legacy filters, writes to them and rewrites
// them, so their images are in the new log.
func goldenLegacyFilters(r *goldenRun) {
	r.ok("BF.ADD", "bf:007", "late")
	r.ok("CF.ADD", "cf:3", "late")
	r.ok("BF.INFO", "bf:wide")
	r.ok("CF.INFO", "cf:7")
	require.NoError(r.t, StartRewrite())
	r.driveRewrite(nil)
	requireRewriteSucceeded(r)
	r.ok("BF.ADD", "bf:hex", "after")
}

type goldenManifest struct {
	SourceRevision string                         `json:"source_revision"`
	Note           string                         `json:"note"`
	Scenarios      map[string]goldenManifestEntry `json:"scenarios"`
}

type goldenManifestEntry struct {
	LogBytes      int    `json:"log_bytes"`
	LogSHA256     string `json:"log_sha256"`
	RepliesSHA256 string `json:"replies_sha256"`
	StateSHA256   string `json:"state_sha256"`
}

func goldenSHA(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func readGoldenFile(t *testing.T, name string) []byte {
	t.Helper()
	f, err := os.Open(filepath.Join(persistenceGoldenDir, name))
	require.NoError(t, err)
	defer f.Close()
	if !strings.HasSuffix(name, ".gz") {
		body, err := io.ReadAll(f)
		require.NoError(t, err)
		return body
	}
	z, err := gzip.NewReader(f)
	require.NoError(t, err)
	body, err := io.ReadAll(z)
	require.NoError(t, err)
	return body
}

func loadGoldenManifest(t *testing.T) goldenManifest {
	t.Helper()
	var m goldenManifest
	require.NoError(t, json.Unmarshal(readGoldenFile(t, "manifest.json"), &m))
	require.Len(t, m.Scenarios, len(goldenScenarios))
	return m
}

// requireSameLog compares two normalized logs and, when they differ, names the
// first record that does.
func requireSameLog(t *testing.T, want, got []byte, what string) {
	t.Helper()
	if bytes.Equal(want, got) {
		return
	}
	a, b := goldenRecords(t, want), goldenRecords(t, got)
	for i := range min(len(a), len(b)) {
		if !slices.Equal(a[i], b[i]) {
			t.Fatalf("%s: record %d differs:\nwant %.200q\ngot  %.200q", what, i, a[i], b[i])
		}
	}
	t.Fatalf("%s: %d records, want %d; the first %d are the same", what, len(b), len(a), min(len(a), len(b)))
}

// TestPersistenceGoldenCapture writes the fixture when asked to, and is
// skipped otherwise. It was run once, on develop at 40eb2f6. Every mode must
// agree before anything is written.
func TestPersistenceGoldenCapture(t *testing.T) {
	dir := os.Getenv("KEEL_CAPTURE_PERSISTENCE_GOLDEN")
	if dir == "" {
		t.Skip("set KEEL_CAPTURE_PERSISTENCE_GOLDEN to rewrite the fixture")
	}
	require.NoError(t, os.MkdirAll(dir, 0o755))
	manifest := goldenManifest{
		SourceRevision: "40eb2f6",
		Note:           "Captured before step 2.3 of the embedding plan moved persistence into core.Engine; see persistence_golden_test.go.",
		Scenarios:      map[string]goldenManifestEntry{},
	}
	for _, scenario := range goldenScenarios {
		var first goldenLog
		for i, mode := range goldenModes {
			got := runGoldenScenario(t, scenario, mode)
			if i == 0 {
				first = got
				continue
			}
			requireSameLog(t, first.log, got.log, scenario.name+" under "+mode.name)
			require.Equal(t, string(first.replies), string(got.replies), "%s replies under %s", scenario.name, mode.name)
			require.Equal(t, string(first.state), string(got.state), "%s state under %s", scenario.name, mode.name)
		}
		var z bytes.Buffer
		w, err := gzip.NewWriterLevel(&z, gzip.BestCompression)
		require.NoError(t, err)
		_, err = w.Write(first.log)
		require.NoError(t, err)
		require.NoError(t, w.Close())
		require.NoError(t, os.WriteFile(filepath.Join(dir, scenario.name+".aof.gz"), z.Bytes(), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, scenario.name+".state.txt"), first.state, 0o644))
		manifest.Scenarios[scenario.name] = goldenManifestEntry{
			LogBytes: len(first.log), LogSHA256: goldenSHA(first.log),
			RepliesSHA256: goldenSHA(first.replies), StateSHA256: goldenSHA(first.state),
		}
	}
	body, err := json.MarshalIndent(manifest, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), append(body, '\n'), 0o644))
}

// TestPersistenceGoldenLogsAreUnchanged: every scenario, under every mode,
// writes develop's log and replies and ends with develop's keyspace.
func TestPersistenceGoldenLogsAreUnchanged(t *testing.T) {
	if os.Getenv("KEEL_CAPTURE_PERSISTENCE_GOLDEN") != "" {
		t.Skip("capturing")
	}
	manifest := loadGoldenManifest(t)
	for _, scenario := range goldenScenarios {
		want, ok := manifest.Scenarios[scenario.name]
		require.True(t, ok, scenario.name)
		wantLog := readGoldenFile(t, scenario.name+".aof.gz")
		require.Equal(t, want.LogSHA256, goldenSHA(wantLog), "the fixture's log")
		wantState := readGoldenFile(t, scenario.name+".state.txt")
		for _, mode := range goldenModes {
			t.Run(scenario.name+"/"+mode.name, func(t *testing.T) {
				got := runGoldenScenario(t, scenario, mode)
				requireSameLog(t, wantLog, got.log, "the log")
				require.Equal(t, want.RepliesSHA256, goldenSHA(got.replies), "the replies")
				require.Equal(t, string(wantState), string(got.state), "the keyspace")
			})
		}
	}
}

// TestPersistenceGoldenLogsReplay: develop's logs replay to develop's
// keyspaces on this build, and so does the log this build rewrites them into.
func TestPersistenceGoldenLogsReplay(t *testing.T) {
	if os.Getenv("KEEL_CAPTURE_PERSISTENCE_GOLDEN") != "" {
		t.Skip("capturing")
	}
	manifest := loadGoldenManifest(t)
	t.Cleanup(ResetStores)
	for _, scenario := range goldenScenarios {
		t.Run(scenario.name, func(t *testing.T) {
			wantState := readGoldenFile(t, scenario.name+".state.txt")
			require.Equal(t, manifest.Scenarios[scenario.name].StateSHA256, goldenSHA(wantState))
			path := filepath.Join(t.TempDir(), "develop.aof")
			require.NoError(t, os.WriteFile(path, readGoldenFile(t, scenario.name+".aof.gz"), 0o644))
			ResetStores()
			_, err := LoadAOF(path)
			require.NoError(t, err)
			never := goldenWindow{start: -1, end: -1 - int64(24*time.Hour/time.Millisecond)}
			require.Equal(t, string(wantState), string(goldenState(t, never)), "develop's log, replayed")

			require.NoError(t, OpenAOF(path))
			t.Cleanup(func() { CloseAOF() })
			require.NoError(t, RewriteAOF())
			require.NoError(t, CloseAOF())
			ResetStores()
			_, err = LoadAOF(path)
			require.NoError(t, err)
			require.Equal(t, string(wantState), string(goldenState(t, never)), "develop's log, rewritten and replayed")
		})
	}
}
