package core

import (
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLatencyMonitorKeepsRedisSamples: an event keeps a sample a second, the
// worse of two in the same second, at most 160, oldest first, and its worst
// since it was reset, as Redis's latencyAddSample keeps them.
func TestLatencyMonitorKeepsRedisSamples(t *testing.T) {
	t.Parallel()
	var m latencyMonitor
	m.add("command", 300, 1000)
	m.add("command", 250, 1000)
	m.add("command", 400, 1000)
	m.add("command", 200, 1002)
	ts := m.events["command"]
	assert.Equal(t, []latencySample{{1000, 400}, {1002, 200}}, ts.inOrder())
	assert.EqualValues(t, 400, ts.max)

	for i := int64(0); i < latencySeriesLen+5; i++ {
		m.add("command", 100+i, 2000+i)
	}
	samples := ts.inOrder()
	require.Len(t, samples, latencySeriesLen)
	assert.Equal(t, latencySample{2005, 105}, samples[0], "the oldest go first")
	assert.Equal(t, latencySample{2000 + latencySeriesLen + 4, 100 + latencySeriesLen + 4}, samples[len(samples)-1])
	assert.EqualValues(t, 400, ts.max, "the worst since a reset, gone from the samples or not")
}

// TestLatencyCommandsAnswerAsRedis: LATEST gives each event's last sample and
// its worst; HISTORY an event's samples, found by its exact name; RESET
// removes the events named without regard to case, or all, and counts them.
func TestLatencyCommandsAnswerAsRedis(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	e.latencyMonitor.add("expire-cycle", 30, 1000)
	e.latencyMonitor.add("command", 300, 1000)
	e.latencyMonitor.add("command", 500, 1002)
	e.latencyMonitor.add("command", 400, 1003)

	assert.Equal(t, []interface{}{
		[]interface{}{"command", int64(1003), int64(400), int64(500)},
		[]interface{}{"expire-cycle", int64(1000), int64(30), int64(30)},
	}, runOn(t, e, "LATENCY", "LATEST"))
	assert.Equal(t, []interface{}{
		[]interface{}{int64(1000), int64(300)}, []interface{}{int64(1002), int64(500)}, []interface{}{int64(1003), int64(400)},
	}, runOn(t, e, "LATENCY", "HISTORY", "command"))
	assert.Equal(t, "*0\r\n", string(rawReplyOn(t, e, "LATENCY", "HISTORY", "COMMAND")), "HISTORY's name is exact")
	assert.Equal(t, "*0\r\n", string(rawReplyOn(t, e, "LATENCY", "HISTORY", "nosuch")))
	assert.Equal(t, "-ERR No samples available for event 'COMMAND'\r\n", string(rawReplyOn(t, e, "LATENCY", "GRAPH", "COMMAND")))

	assert.EqualValues(t, 0, runOn(t, e, "LATENCY", "RESET", "blabla"))
	assert.EqualValues(t, 1, runOn(t, e, "LATENCY", "RESET", "COMMAND", "nosuch"), "RESET's is not")
	assert.Equal(t, []interface{}{[]interface{}{"expire-cycle", int64(1000), int64(30), int64(30)}}, runOn(t, e, "LATENCY", "LATEST"))
	e.latencyMonitor.add("command", 1, 1004)
	assert.EqualValues(t, 2, runOn(t, e, "LATENCY", "RESET"))
	assert.Equal(t, "*0\r\n", string(rawReplyOn(t, e, "LATENCY", "LATEST")))
}

// TestLatencyGraphIsRedis: LATENCY GRAPH draws what Redis 8.10.2 drew for the
// three samples of latency-monitor.tcl's DEBUG SLEEPs, 0.3, 0.4 and 0.5
// seconds, taken in the second of the last.
func TestLatencyGraphIsRedis(t *testing.T) {
	t.Parallel()
	var m latencyMonitor
	now := int64(1792040617)
	m.add("command", 301, now-3)
	m.add("command", 400, now-1)
	m.add("command", 500, now)
	want := "command - high 500 ms, low 301 ms (all time high 500 ms)\n" +
		strings.Repeat("-", 80) + "\n" +
		"  #\n" +
		"  |\n" +
		" #|\n" +
		"_||\n" +
		"   \n" +
		"310\n" +
		"sss\n"
	assert.Equal(t, want, latencyGraph("command", m.events["command"], now))

	// Labels count minutes, hours and days, and a run of more than 80
	// samples goes on in a second block. Equal samples draw at the bottom.
	var long latencyMonitor
	for i := int64(0); i < 82; i++ {
		long.add("x", 7, now-86400-i*60)
	}
	graph := latencyGraph("x", long.events["x"], now)
	lines := strings.Split(graph, "\n")
	assert.Equal(t, strings.Repeat("_", 80), lines[5])
	assert.Equal(t, strings.Repeat("1", 80), lines[7], "a day and some minutes ago, oldest first")
	assert.Equal(t, strings.Repeat("d", 80), lines[8])
	assert.Equal(t, "", lines[9], "the blank line between blocks")
	assert.Equal(t, "__", lines[13])
}

// TestLatencyDoctorIsRedis: LATENCY DOCTOR's report is Redis 8.10.2's for the
// same samples and settings: its spikes, then its advice; no spike at all
// once reset, and the monitor off once its threshold is 0 again.
func TestLatencyDoctorIsRedis(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "CONFIG", "SET", "latency-monitor-threshold", "200")
	runOn(t, e, "CONFIG", "SET", "slowlog-log-slower-than", "1000000")
	now := int64(1792040617)
	e.latencyMonitor.add("command", 301, now-3)
	e.latencyMonitor.add("command", 400, now-1)
	e.latencyMonitor.add("command", 500, now)
	assert.Equal(t, "Dave, I have observed latency spikes in this Redis instance. You don't mind talking about it, do you Dave?\n\n"+
		"1. command: 3 latency spikes (average 400ms, mean deviation 66ms, period 1.00 sec). Worst all time event 500ms.\n"+
		"\nI have a few advices for you:\n\n"+
		"- Your current Slow Log configuration only logs events that are slower than your configured latency monitor threshold. Please use 'CONFIG SET slowlog-log-slower-than 200000'.\n"+
		"- Check your Slow Log to understand what are the commands you are running which are too slow to execute. Please check https://redis.io/commands/slowlog for more information.\n"+
		"- Deleting, expiring or evicting (because of maxmemory policy) large objects is a blocking operation. If you have very large objects that are often deleted, expired, or evicted, try to fragment those objects into multiple smaller objects.\n",
		e.latencyReport(now, 0))

	report := e.latencyReport(now, 2<<20)
	assert.True(t, strings.HasSuffix(report, latencyAdvice[adviseDisableTHP]), "anonymous huge pages add their advice, last")

	runOn(t, e, "LATENCY", "RESET")
	assert.Equal(t, "Dave, no latency spike was observed during the lifetime of this Redis instance, not in the slightest bit. "+
		"I honestly think you ought to sit down calmly, take a stress pill, and think things over.\n", e.latencyReport(now, 0))
	runOn(t, e, "CONFIG", "SET", "latency-monitor-threshold", "0")
	assert.Equal(t, latencyDoctorDisabled, e.latencyReport(now, 0))
	assert.Regexp(t, `^=\d+\r\ntxt:I'm sorry, Dave`, string(rawReplyAsOn(t, e, true, "LATENCY", "DOCTOR")))
}

// TestLatencyDoctorAdvisesOnEachEvent: the advice each event brings, with
// those Redis gives only under appendfsync always and an hz below 100.
func TestLatencyDoctorAdvisesOnEachEvent(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{Fsync: FsyncAlways})
	e.SetServerInfo(&ServerInfo{Hz: 10})
	runOn(t, e, "CONFIG", "SET", "latency-monitor-threshold", "5")
	runOn(t, e, "CONFIG", "SET", "slowlog-log-slower-than", "-1")
	for _, event := range []string{"command", "fast-command", "aof-write-pending-fsync", "aof-write-active-child",
		"aof-fsync-always", "expire-cycle", "eviction-cycle"} {
		e.latencyMonitor.add(event, 9, 1000)
	}
	report := e.latencyReport(1010, 0)
	assert.Contains(t, report, "7. fast-command: 1 latency spikes (average 9ms, mean deviation 0ms, period 10.00 sec). Worst all time event 9ms.\n")
	advice := report[strings.Index(report, "advices for you:\n\n")+len("advices for you:\n\n"):]
	var want strings.Builder
	for _, kind := range []int{adviseSlowlogEnabled, adviseSlowlogInspect, adviseScheduler, adviseLocalDisk, adviseSSD,
		adviseDataWriteback, adviseDiskContention, adviseNoAppendfsync, adviseRelaxFsyncPolicy, adviseHz,
		adviseLargeObjects, adviseMassEviction} {
		want.WriteString(latencyAdvice[kind])
	}
	assert.Equal(t, strings.Replace(want.String(), "%llu", "5000", 1), advice)

	e.serverInfo.Hz = 100
	assert.NotContains(t, e.latencyReport(1010, 0), "'hz'")
	e.SetServerInfo(nil)
	assert.NotContains(t, e.latencyReport(1010, 0), "'hz'", "no server, no hz to set")
}

// TestCommandsTakeTheirLatencyEvent: a command that took the threshold or
// more is a fast-command if its spec carries FAST, as GET's does, and a
// command if not; one that took less is nothing, and with the threshold 0
// nothing is recorded. The slow log keeps its own bound.
func TestCommandsTakeTheirLatencyEvent(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.EqualValues(t, 10000, e.longFrom, "the slow log's default bound")
	ago := func(d time.Duration) int64 { return nanotime() - int64(d) }
	get, keys := &Command{Cmd: "GET", Args: []string{"k"}}, &Command{Cmd: "KEYS", Args: []string{"*"}}

	e.noteRun(keys, statOf(keys), ago(50*time.Millisecond), false)
	assert.Empty(t, e.latencyMonitor.events, "off by default")
	assert.Len(t, e.slowlog.entries, 1)

	runOn(t, e, "CONFIG", "SET", "latency-monitor-threshold", "2")
	assert.EqualValues(t, 2000, e.longFrom)
	assert.Equal(t, "2", runOn(t, e, "CONFIG", "GET", "latency-monitor-threshold").([]interface{})[1])
	e.noteRun(get, statOf(get), ago(5*time.Millisecond), false)
	e.noteRun(keys, statOf(keys), ago(3*time.Millisecond), false)
	e.noteRun(get, statOf(get), ago(time.Millisecond), false)
	require.Contains(t, e.latencyMonitor.events, "fast-command")
	require.Contains(t, e.latencyMonitor.events, "command")
	assert.GreaterOrEqual(t, e.latencyMonitor.events["fast-command"].max, int64(5))
	assert.Len(t, e.latencyMonitor.events["command"].inOrder(), 1)
	assert.Len(t, e.slowlog.entries, 1, "under the slow log's 10 ms")

	runOn(t, e, "CONFIG", "SET", "slowlog-log-slower-than", "1000")
	assert.EqualValues(t, 1000, e.longFrom, "the lower of the two")
	runOn(t, e, "CONFIG", "SET", "slowlog-log-slower-than", "-1")
	runOn(t, e, "CONFIG", "SET", "latency-monitor-threshold", "0")
	assert.EqualValues(t, int64(math.MaxInt64), e.longFrom, "neither is on")
	runOn(t, e, "CONFIG", "SET", "latency-monitor-threshold", "9223372036854775807")
	assert.EqualValues(t, int64(math.MaxInt64), e.longFrom, "a threshold past what microseconds hold is never reached")
}

// TestLogWritesAndSyncsTakeTheirLatencyEvents: a write to the log that took
// the threshold or more is aof-write, and aof-write-alone with no sync or
// rewrite under way; a sync under appendfsync always is aof-fsync-always.
func TestLogWritesAndSyncsTakeTheirLatencyEvents(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{Fsync: FsyncAlways})
	runOn(t, e, "CONFIG", "SET", "latency-monitor-threshold", "1")
	write, sync := e.aofWrite, e.aofSync
	e.aofWrite = func(f *os.File, b []byte) (int, error) {
		time.Sleep(3 * time.Millisecond)
		return write(f, b)
	}
	e.aofSync = func(f *os.File) error {
		time.Sleep(3 * time.Millisecond)
		return sync(f)
	}
	withAOFOn(t, e, func() {
		runOn(t, e, "SET", "k", "v")
		require.NoError(t, e.FlushAOF())
	})
	for _, event := range []string{"aof-write", "aof-write-alone", "aof-fsync-always"} {
		require.Contains(t, e.latencyMonitor.events, event)
		assert.GreaterOrEqual(t, e.latencyMonitor.events[event].max, int64(3), event)
	}
	assert.NotContains(t, e.latencyMonitor.events, "aof-write-pending-fsync")
}

// TestExpireCycleTakesItsLatencyEvent: a cycle that took the threshold or
// more is expire-cycle, and one that took less is nothing.
func TestExpireCycleTakesItsLatencyEvent(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	for i := 0; i < 50000; i++ {
		runOn(t, e, "SET", "k"+strconv.Itoa(i), "v", "PX", "1")
	}
	waitPast(20)
	runOn(t, e, "CONFIG", "SET", "latency-monitor-threshold", "1")
	recorded := 0
	for e.KeysWithExpiry() > 0 {
		before := e.expireCycleUsec
		e.ExpireCycle()
		took := int64(e.expireCycleUsec-before) / 1000
		var samples []latencySample
		if ts := e.latencyMonitor.events["expire-cycle"]; ts != nil {
			samples = ts.inOrder()
		}
		if took >= 1 {
			require.Len(t, samples, recorded+1, "a cycle of %d ms", took)
			assert.Equal(t, took, samples[recorded].latency)
			recorded++
			e.latencyMonitor.reset("", true)
			recorded = 0
		} else {
			require.Len(t, samples, recorded, "a cycle of under a millisecond")
		}
	}
}
