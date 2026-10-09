package core

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cmdstats reads INFO commandstats on e into each command's fields.
func cmdstats(t testing.TB, e *Engine) map[string]map[string]string {
	t.Helper()
	out := map[string]map[string]string{}
	for name, value := range infoFields(t, runOn(t, e, "INFO", "commandstats").(string)) {
		fields := map[string]string{}
		for _, part := range strings.Split(value, ",") {
			k, v, _ := strings.Cut(part, "=")
			fields[k] = v
		}
		out[strings.TrimPrefix(name, "cmdstat_")] = fields
	}
	return out
}

// TestCommandTimeIsRedisWholeMicroseconds: a command's time is the
// difference of its start and end, each in whole microseconds, as Redis's
// call() takes ustime() before and after it.
func TestCommandTimeIsRedisWholeMicroseconds(t *testing.T) {
	t.Parallel()
	assert.Equal(t, uint64(0), microsBetween(10_100, 10_900), "within one microsecond")
	assert.Equal(t, uint64(1), microsBetween(10_900, 11_100), "across a microsecond boundary")
	assert.Equal(t, uint64(3), microsBetween(1_000, 4_999))
}

// TestCommandstatsAreRedisLines: one line for each command that ran or was
// refused, under Redis's names, subcommands after a '|', with usec_per_call
// computed in single precision and written with two decimals.
func TestCommandstatsAreRedisLines(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "SET", "k", "v")
	runOn(t, e, "GET", "k")
	runOn(t, e, "GET")             // refused: the wrong count
	runOn(t, e, "LPUSH", "k", "x") // failed: the wrong type
	runOn(t, e, "CONFIG", "GET", "maxmemory")
	runOn(t, e, "CONFIG", "GET")    // refused, as config|get
	runOn(t, e, "CONFIG", "nosuch") // no line: Redis has no such command
	runOn(t, e, "NOSUCH")           // no line either
	stats := cmdstats(t, e)
	assert.Equal(t, "1", stats["set"]["calls"])
	assert.Equal(t, map[string]string{"calls": "1", "rejected_calls": "1", "failed_calls": "0"},
		pick(stats["get"], "calls", "rejected_calls", "failed_calls"))
	assert.Equal(t, map[string]string{"calls": "1", "rejected_calls": "0", "failed_calls": "1"},
		pick(stats["lpush"], "calls", "rejected_calls", "failed_calls"))
	assert.Equal(t, map[string]string{"calls": "1", "rejected_calls": "1", "failed_calls": "0"},
		pick(stats["config|get"], "calls", "rejected_calls", "failed_calls"))
	assert.NotContains(t, stats, "config")
	assert.NotContains(t, stats, "nosuch")
	assert.NotContains(t, stats, "info", "INFO is counted once it has answered")
	for name, fields := range stats {
		calls, _ := strconv.ParseUint(fields["calls"], 10, 64)
		usec, _ := strconv.ParseUint(fields["usec"], 10, 64)
		want := "0.00"
		if calls != 0 {
			want = strconv.FormatFloat(float64(float32(usec)/float32(calls)), 'f', 2, 64)
		}
		assert.Equal(t, want, fields["usec_per_call"], name)
	}

	e.cmdStats[statOf(&Command{Cmd: "SET"})].usec = 16777217
	e.cmdStats[statOf(&Command{Cmd: "SET"})].calls = 3
	line := regexp.MustCompile(`cmdstat_set:[^\r]*`).FindString(runOn(t, e, "INFO", "commandstats").(string))
	assert.Equal(t, "cmdstat_set:calls=3,usec=16777217,usec_per_call=5592405.50,rejected_calls=0,failed_calls=0", line,
		"16777217 is 16777216 in single precision, as Redis divides it")
}

func pick(m map[string]string, keys ...string) map[string]string {
	out := map[string]string{}
	for _, k := range keys {
		out[k] = m[k]
	}
	return out
}

// TestCommandstatsSurviveReplayAsRedisCounts: replaying the log counts the
// commands of its MULTI blocks, which Redis's EXEC runs through call(), and
// nothing else; CONFIG RESETSTAT clears every line.
func TestCommandstatsSurviveReplayAsRedisCounts(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "replay.aof")
	var log []byte
	for _, cmd := range [][]string{{"SET", "a", "1"}, {"MULTI"}, {"SET", "b", "2"}, {"INCR", "a"}, {"EXEC"}, {"SET", "c", "3"}} {
		log = append(log, encodeStringArray(cmd)...)
	}
	require.NoError(t, os.WriteFile(path, log, 0o600))
	e := newTestEngine(t, Options{})
	_, err := e.LoadAOF(path)
	require.NoError(t, err)
	stats := cmdstats(t, e)
	assert.Equal(t, "1", stats["set"]["calls"], "the block's SET only")
	assert.Equal(t, "1", stats["incr"]["calls"])

	assert.Equal(t, "OK", runOn(t, e, "CONFIG", "RESETSTAT"))
	stats = cmdstats(t, e)
	assert.Equal(t, []string{"config|resetstat"}, keys(stats), "RESETSTAT itself, and INFO once it has answered")
}

func keys(m map[string]map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}
