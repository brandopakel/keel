package core

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLatencystatsAreRedisLines: a line for each command that has run, at
// Redis's default percentiles, in microseconds with three decimals; in INFO
// all and latencystats, not the default; gone after CONFIG RESETSTAT.
func TestLatencystatsAreRedisLines(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "SET", "k", "v")
	runOn(t, e, "CONFIG", "GET", "maxmemory")
	runOn(t, e, "GET") // refused before it ran: no time
	info := runOn(t, e, "INFO", "latencystats").(string)
	assert.Regexp(t, `\r\nlatency_percentiles_usec_set:p50=\d+\.\d{3},p99=\d+\.\d{3},p99\.9=\d+\.\d{3}\r\n`, info)
	assert.Contains(t, info, "latency_percentiles_usec_config|get:")
	assert.NotContains(t, info, "latency_percentiles_usec_get:")
	assert.NotContains(t, runOn(t, e, "INFO").(string), "# Latencystats")
	assert.Contains(t, runOn(t, e, "INFO", "all").(string), "# Latencystats")

	e.latency[statOf(&Command{Cmd: "SET"})] = newLatencyHistogram()
	for _, micros := range []uint64{1, 2, 3, 150} {
		e.recordLatency(statOf(&Command{Cmd: "SET"}), micros)
	}
	line := regexp.MustCompile(`latency_percentiles_usec_set:[^\r]*`).FindString(runOn(t, e, "INFO", "latencystats").(string))
	assert.Equal(t, "latency_percentiles_usec_set:p50=2.007,p99=150.527,p99.9=150.527", line, "Redis's HdrHistogram's answer")

	runOn(t, e, "CONFIG", "RESETSTAT")
	assert.NotContains(t, runOn(t, e, "INFO", "latencystats").(string), "latency_percentiles_usec_set:")
}

// TestLatencyAnswersAsRedisWithItsMonitorOff: the latency monitor's
// subcommands answer as Redis's do with latency-monitor-threshold 0 and no
// event ever recorded; HISTOGRAM gives each command's calls and buckets.
func TestLatencyAnswersAsRedisWithItsMonitorOff(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.Equal(t, "*0\r\n", string(rawReplyOn(t, e, "LATENCY", "LATEST")))
	assert.Equal(t, "*0\r\n", string(rawReplyOn(t, e, "LATENCY", "HISTORY", "command")))
	assert.Equal(t, ":0\r\n", string(rawReplyOn(t, e, "LATENCY", "RESET")))
	assert.Equal(t, ":0\r\n", string(rawReplyOn(t, e, "LATENCY", "RESET", "a", "b")))
	assert.Equal(t, "-ERR No samples available for event 'command'\r\n", string(rawReplyOn(t, e, "LATENCY", "GRAPH", "command")))
	assert.Equal(t, latencyDoctorDisabled, runOn(t, e, "LATENCY", "DOCTOR"))
	assert.Regexp(t, `^=\d+\r\ntxt:I'm sorry, Dave`, string(rawReplyAsOn(t, e, true, "LATENCY", "DOCTOR")))
	help := runOn(t, e, "LATENCY", "HELP").([]interface{})
	assert.Equal(t, "LATENCY <subcommand> [<arg> [value] [opt] ...]. Subcommands are:", help[0])
	for args, want := range map[string]string{
		"HISTORY":   "-ERR wrong number of arguments for 'latency|history' command\r\n",
		"LATEST x":  "-ERR wrong number of arguments for 'latency|latest' command\r\n",
		"nosuch":    "-ERR unknown subcommand 'nosuch'. Try LATENCY HELP.\r\n",
		"GRAPH a b": "-ERR wrong number of arguments for 'latency|graph' command\r\n",
	} {
		assert.Equal(t, want, string(rawReplyOn(t, e, "LATENCY", splitArgs(args)...)), args)
	}

	runOn(t, e, "SET", "k", "v")
	runOn(t, e, "SET", "k", "w")
	runOn(t, e, "CONFIG", "GET", "maxmemory")
	all := runOn(t, e, "LATENCY", "HISTOGRAM").([]interface{})
	got := map[string]interface{}{}
	for i := 0; i < len(all); i += 2 {
		got[all[i].(string)] = all[i+1]
	}
	require.Contains(t, got, "set")
	set := got["set"].([]interface{})
	assert.Equal(t, []interface{}{"calls", int64(2), "histogram_usec"}, set[:3])
	buckets := set[3].([]interface{})
	require.NotEmpty(t, buckets)
	assert.Equal(t, int64(2), buckets[len(buckets)-1], "the last bucket counts every call")
	assert.Contains(t, got, "config|get")

	named := runOn(t, e, "LATENCY", "HISTOGRAM", "SET", "config", "nosuch", "CONFIG|GET").([]interface{})
	var names []string
	for i := 0; i < len(named); i += 2 {
		names = append(names, named[i].(string))
	}
	assert.Equal(t, []string{"set", "config|get", "config|get"}, names, "a container brings its subcommands; an unknown name nothing")
	assert.Regexp(t, `^%1\r\n\$3\r\nset\r\n%2\r\n`, string(rawReplyAsOn(t, e, true, "LATENCY", "HISTOGRAM", "set")))
}

func splitArgs(s string) []string { return regexp.MustCompile(`\s+`).Split(s, -1) }

// TestLatencySettingsAreRedisDefaults: CONFIG GET reports Redis's defaults,
// which are what Keel does.
func TestLatencySettingsAreRedisDefaults(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.Equal(t, [][2]string{{"latency-tracking", "yes"}}, configPairs(t, e, "latency-tracking"))
	assert.Equal(t, [][2]string{{"latency-tracking-info-percentiles", "50 99 99.9"}}, configPairs(t, e, "latency-tracking-info-percentiles"))
	assert.Equal(t, [][2]string{{"latency-monitor-threshold", "0"}}, configPairs(t, e, "latency-monitor-threshold"))
}

// TestLatencyKeepsWhatReplayKeeps: the log's replay records no latency, a
// MULTI block's commands' included, as Redis's call() records none while
// its log loads.
func TestLatencyKeepsWhatReplayKeeps(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "replay.aof")
	var log []byte
	for _, cmd := range [][]string{{"SET", "a", "1"}, {"MULTI"}, {"SET", "b", "2"}, {"EXEC"}, {"HSET", "h", "f", "v"}} {
		log = append(log, encodeStringArray(cmd)...)
	}
	require.NoError(t, os.WriteFile(path, log, 0o600))
	e := newTestEngine(t, Options{})
	_, err := e.LoadAOF(path)
	require.NoError(t, err)
	assert.Nil(t, e.latency[statOf(&Command{Cmd: "SET"})], "not even the block's SET")
	assert.Nil(t, e.latency[statOf(&Command{Cmd: "HSET"})])
}
