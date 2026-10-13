package core

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// statsOn reads INFO stats and errorstats on e, which INFO itself adds to
// once it has answered.
func statsOn(t testing.TB, e *Engine) map[string]string {
	t.Helper()
	return infoFields(t, runOn(t, e, "INFO", "stats", "errorstats").(string))
}

// TestCommandsCountAsRedisCallCountsThem: a command counts once it has run,
// so INFO does not count itself; a refusal before it runs does not count,
// and a failure as it runs does. Every error reply counts, under its prefix.
func TestCommandsCountAsRedisCallCountsThem(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	stats := statsOn(t, e)
	assert.Equal(t, "0", stats["total_commands_processed"], "INFO is counted after it answers")
	assert.Equal(t, "0", stats["total_error_replies"])

	runOn(t, e, "SET", "k", "v")
	runOn(t, e, "GET", "k")
	runOn(t, e, "NOSUCH")           // unknown: refused, an ERR
	runOn(t, e, "GET")              // the wrong count: refused, an ERR
	runOn(t, e, "LPUSH", "k", "x")  // the wrong type: ran and failed, a WRONGTYPE
	runOn(t, e, "INCR", "k")        // not an integer: ran and failed, an ERR
	runOn(t, e, "CONFIG", "nosuch") // an unknown subcommand: refused, an ERR
	stats = statsOn(t, e)
	// The first INFO, SET, GET, LPUSH and INCR.
	assert.Equal(t, "5", stats["total_commands_processed"])
	assert.Equal(t, "5", stats["total_error_replies"])
	assert.Equal(t, "count=4", stats["errorstat_ERR"])
	assert.Equal(t, "count=1", stats["errorstat_WRONGTYPE"])
}

// TestErrorstatsKeepRedisPrefixes: the word after the '-' when a space ends it
// within 32 bytes, ERR otherwise; the characters INFO cannot carry as
// underscores; at most 128 prefixes, then only ERRORSTATS_DISABLED, count 1,
// until CONFIG RESETSTAT, while total_error_replies goes on counting.
func TestErrorstatsKeepRedisPrefixes(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	e.noteError([]byte("-NOAUTH Authentication required.\r\n"))
	e.noteError([]byte("-" + strings.Repeat("X", 40) + " too long to be a prefix\r\n"))
	e.noteError([]byte("-ERR\r\n"))
	e.noteError([]byte("-A:B c\r\n"))
	stats := statsOn(t, e)
	assert.Equal(t, "count=1", stats["errorstat_NOAUTH"])
	assert.Equal(t, "count=2", stats["errorstat_ERR"], "no space within 32 bytes")
	assert.Equal(t, "count=1", stats["errorstat_A_B"], "INFO cannot carry ':'")
	assert.Equal(t, "4", stats["total_error_replies"])

	for i := len(e.totals.errorPrefixes); i < maxErrorPrefixes; i++ {
		e.noteError([]byte(fmt.Sprintf("-P%d x\r\n", i)))
	}
	require.Len(t, e.totals.errorPrefixes, maxErrorPrefixes)
	e.noteError([]byte("-ERR x\r\n")) // a prefix Redis already has
	assert.Len(t, e.totals.errorPrefixes, maxErrorPrefixes)
	e.noteError([]byte("-NEW x\r\n"))
	e.noteError([]byte("-ERR x\r\n"))
	stats = statsOn(t, e)
	assert.Equal(t, "count=1", stats[errorstat(errorStatsDisabled)], "Redis stops counting by prefix")
	assert.NotContains(t, stats, "errorstat_ERR")
	assert.Equal(t, fmt.Sprint(maxErrorPrefixes+4), stats["total_error_replies"])

	assert.Equal(t, "OK", runOn(t, e, "CONFIG", "RESETSTAT"))
	e.noteError([]byte("-NEW x\r\n"))
	stats = statsOn(t, e)
	assert.Equal(t, "count=1", stats["errorstat_NEW"], "RESETSTAT keeps prefixes again")
	assert.NotContains(t, stats, errorstat(errorStatsDisabled))
}

func errorstat(prefix string) string { return "errorstat_" + prefix }

// TestTransactionsCountAsRedisCountsThem: MULTI, EXEC and DISCARD count as
// they run; what MULTI queues counts when EXEC runs it, and not when queued;
// a refusal while queueing and the EXECABORT it leads to are errors.
func TestTransactionsCountAsRedisCountsThem(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	var tx *Transaction
	var w replyWriter
	transact := func(name string, args ...string) {
		t.Helper()
		var err error
		tx, err = e.Transact(tx, &Command{Cmd: name, Args: args}, &w, nil)
		require.NoError(t, err)
	}
	transact("MULTI")
	transact("SET", "k", "v")
	transact("LPUSH", "k", "x") // fails as it runs, inside EXEC's reply
	assert.Equal(t, uint64(1), e.totals.commands, "queued, not run")
	transact("EXEC")
	assert.Equal(t, uint64(4), e.totals.commands, "MULTI, SET, LPUSH and EXEC")
	assert.Equal(t, map[string]uint64{"WRONGTYPE": 1}, e.totals.errorPrefixes)

	transact("MULTI")
	transact("GET") // refused while queueing: the transaction is aborted
	transact("EXEC")
	assert.Equal(t, uint64(6), e.totals.commands, "MULTI and EXEC, not the refused GET")
	assert.Equal(t, map[string]uint64{"WRONGTYPE": 1, "ERR": 1, "EXECABORT": 1}, e.totals.errorPrefixes)

	transact("DISCARD") // without MULTI: runs, and fails
	assert.Equal(t, uint64(7), e.totals.commands)
	assert.Equal(t, uint64(4), e.totals.errors)
}

// TestReplayCountsWhatRedisReplayCounts: replaying the log counts none of
// its commands but those of a MULTI block, which Redis's EXEC runs through
// call(); nothing the engine counted before is lost.
func TestReplayCountsWhatRedisReplayCounts(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "replay.aof")
	var log []byte
	for _, cmd := range [][]string{{"SET", "a", "1"}, {"MULTI"}, {"SET", "b", "2"}, {"INCR", "a"}, {"EXEC"}, {"SET", "c", "3"}} {
		log = append(log, encodeStringArray(cmd)...)
	}
	require.NoError(t, os.WriteFile(path, log, 0o600))
	e := newTestEngine(t, Options{})
	e.noteError([]byte("-ERR before\r\n"))
	_, err := e.LoadAOF(path)
	require.NoError(t, err)
	assert.Equal(t, uint64(2), e.totals.commands, "the block's SET and INCR")
	assert.Equal(t, map[string]uint64{"ERR": 1}, e.totals.errorPrefixes)
	assert.Equal(t, "2", runOn(t, e, "GET", "a"))
}

// TestInstantaneousOpsSamplesAsRedisDoes: a sample at most every 100 ms, of
// the rate since the one before, and the mean of the last 16 reported.
func TestInstantaneousOpsSamplesAsRedisDoes(t *testing.T) {
	t.Parallel()
	var m instantaneousMetric
	start := time.Unix(1000, 0)
	m.sample(0, start)
	assert.Zero(t, m.rate(), "the first sample only sets where the next counts from")
	m.sample(500, start.Add(50*time.Millisecond))
	assert.Zero(t, m.rate(), "too soon for another sample")
	m.sample(1000, start.Add(100*time.Millisecond))
	assert.Equal(t, int64(10000/16), m.rate(), "1000 in 100 ms, one sample of 16")
	for i := 2; i <= 17; i++ {
		m.sample(uint64(1000*i), start.Add(time.Duration(i)*100*time.Millisecond))
	}
	assert.Equal(t, int64(10000), m.rate(), "16 samples of 10000 a second")
}

// TestResetStatResetsWhatRedisResets: CONFIG RESETSTAT zeroes the counts
// Redis's resetServerStats zeroes that Keel reports, the transport's through
// its hook, and then counts as the first command, as Redis counts it; the
// memory peak, which Redis keeps, stays.
func TestResetStatResetsWhatRedisResets(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	transportReset := 0
	e.SetStatsReset(func() { transportReset++ })
	runOn(t, e, "SET", "k", "v")
	runOn(t, e, "NOSUCH")
	e.expiredKeys, e.aof.rewrites, e.rewriteOutcome.failures = 3, 4, 5
	e.totals.ops.samples[0] = 99
	peak := e.NoteMemoryPeak()
	require.NotZero(t, peak)

	assert.Equal(t, "OK", runOn(t, e, "CONFIG", "RESETSTAT"))
	assert.Equal(t, 1, transportReset)
	stats := statsOn(t, e)
	assert.Equal(t, "1", stats["total_commands_processed"], "CONFIG RESETSTAT itself")
	assert.Equal(t, "0", stats["instantaneous_ops_per_sec"])
	assert.Equal(t, "0", stats["total_error_replies"])
	assert.Equal(t, "0", stats["expired_keys"])
	assert.Equal(t, "0", stats["evicted_keys"])
	assert.NotContains(t, stats, "errorstat_ERR")
	assert.Zero(t, e.aof.rewrites)
	assert.Zero(t, e.rewriteOutcome.failures)
	assert.Equal(t, peak, e.memoryPeak, "Redis keeps used_memory_peak")
}
