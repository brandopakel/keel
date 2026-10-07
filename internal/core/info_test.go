package core

import (
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// infoFields reads an INFO reply into its fields, by name.
func infoFields(t testing.TB, info string) map[string]string {
	t.Helper()
	fields := map[string]string{}
	for _, line := range strings.Split(info, "\r\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		require.True(t, ok, "a line that is not name:value: %q", line)
		fields[name] = value
	}
	return fields
}

// infoSteady is an INFO reply with the values that move with the clock - the
// time, the uptime, the processor time and the resident memory - masked, so
// that two replies can be compared.
func infoSteady(info string) string {
	return regexp.MustCompile(`(?m)^(server_time_usec|uptime_in_seconds|uptime_in_days|used_cpu_[a-z_]+|used_memory_rss(_human)?):.*$`).
		ReplaceAllString(info, "$1:*")
}

// infoSectionNames lists an INFO reply's sections in order.
func infoSectionNames(info string) []string {
	var names []string
	for _, line := range strings.Split(info, "\r\n") {
		if name, ok := strings.CutPrefix(line, "# "); ok {
			names = append(names, name)
		}
	}
	return names
}

// TestINFOSectionsComeInRedisOrder: the sections are Redis's, in its order,
// with Clients only where a transport reports its connections and CPU only
// where the platform reports processor time.
func TestINFOSectionsComeInRedisOrder(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	want := []string{"Server", "Memory", "Persistence", "Stats", "Replication", "CPU", "Cluster", "Keyspace"}
	if runtime.GOOS == "windows" {
		want = []string{"Server", "Memory", "Persistence", "Stats", "Replication", "Cluster", "Keyspace"}
	}
	assert.Equal(t, want, infoSectionNames(runOn(t, e, "INFO").(string)))

	e.SetClientBuffers(func() ClientBufferStats { return ClientBufferStats{Connected: 1} })
	got := infoSectionNames(runOn(t, e, "INFO").(string))
	assert.Equal(t, append([]string{"Server", "Clients"}, want[1:]...), got)
	assert.Equal(t, "cluster_enabled:0", strings.TrimSpace(strings.TrimPrefix(runOn(t, e, "INFO", "cluster").(string), "# Cluster")))
}

// TestINFOServerReportsRedisFields: the Server section's fields as Redis
// writes them, and the transport's own only once it has said what they are.
func TestINFOServerReportsRedisFields(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	other := newTestEngine(t, Options{})
	before := time.Now()
	fields := infoFields(t, runOn(t, e, "INFO", "server").(string))

	assert.Equal(t, RedisCompatibleVersion, fields["redis_version"])
	assert.Equal(t, strconv.Itoa(strconv.IntSize), fields["arch_bits"])
	assert.NotEmpty(t, fields["os"])
	assert.Equal(t, strconv.Itoa(os.Getpid()), fields["process_id"])
	assert.Equal(t, "no", fields["process_supervised"])
	assert.Regexp(t, `^[0-9a-f]{40}$`, fields["run_id"])
	assert.Equal(t, fields["run_id"], infoFields(t, runOn(t, e, "INFO", "server").(string))["run_id"], "one engine keeps its run_id")
	assert.NotEqual(t, fields["run_id"], infoFields(t, runOn(t, other, "INFO", "server").(string))["run_id"])
	usec, err := strconv.ParseInt(fields["server_time_usec"], 10, 64)
	require.NoError(t, err)
	assert.InDelta(t, before.UnixMicro(), usec, float64(time.Minute/time.Microsecond))
	assert.Equal(t, "0", fields["uptime_in_days"])
	_, err = strconv.Atoi(fields["uptime_in_seconds"])
	assert.NoError(t, err)
	assert.Contains(t, fields, "config_file")
	for _, name := range []string{"tcp_port", "hz", "configured_hz", "multiplexing_api", "io_threads_active"} {
		assert.NotContains(t, fields, name, "no transport has said")
	}

	e.SetServerInfo(&ServerInfo{Port: 6390, MaxClients: 50, IOThreads: 4, Hz: 10, Multiplexer: "epoll"})
	fields = infoFields(t, runOn(t, e, "INFO", "server").(string))
	assert.Equal(t, "6390", fields["tcp_port"])
	assert.Equal(t, "10", fields["hz"])
	assert.Equal(t, "10", fields["configured_hz"])
	assert.Equal(t, "epoll", fields["multiplexing_api"])
	assert.Equal(t, "1", fields["io_threads_active"])
	e.SetClientBuffers(func() ClientBufferStats { return ClientBufferStats{} })
	assert.Equal(t, "50", infoFields(t, runOn(t, e, "INFO", "clients").(string))["maxclients"])
	e.SetServerInfo(nil)
	assert.NotContains(t, infoFields(t, runOn(t, e, "INFO", "server").(string)), "tcp_port")
}

// TestINFOMemoryPeakOutlivesWhatWasStored: used_memory_peak is the most the
// stores have held, which deleting what they hold does not lower.
func TestINFOMemoryPeakOutlivesWhatWasStored(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	start := time.Now().Unix()
	fields := infoFields(t, runOn(t, e, "INFO", "memory").(string))
	assert.Equal(t, "0", fields["used_memory_peak"])
	assert.Equal(t, "100.00%", fields["used_memory_peak_perc"])

	for i := 0; i < 100; i++ {
		runOn(t, e, "SET", "key:"+strconv.Itoa(i), strings.Repeat("v", 100))
	}
	e.NoteMemoryPeak()
	full := infoFields(t, runOn(t, e, "INFO", "memory").(string))
	assert.Equal(t, full["used_memory"], full["used_memory_peak"])
	assert.Equal(t, "100.00%", full["used_memory_peak_perc"])
	peakTime, err := strconv.ParseInt(full["used_memory_peak_time"], 10, 64)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, peakTime, start)

	runOn(t, e, "FLUSHDB")
	after := infoFields(t, runOn(t, e, "INFO", "memory").(string))
	assert.Equal(t, full["used_memory_peak"], after["used_memory_peak"], "a flush does not lower the peak")
	used, _ := strconv.ParseUint(after["used_memory"], 10, 64)
	peak, _ := strconv.ParseUint(after["used_memory_peak"], 10, 64)
	assert.Less(t, used, peak)
	percent, err := strconv.ParseFloat(strings.TrimSuffix(after["used_memory_peak_perc"], "%"), 64)
	require.NoError(t, err)
	assert.Less(t, percent, 100.0)
	for _, name := range []string{"active_defrag_running", "lazyfree_pending_objects", "lazyfreed_objects"} {
		assert.Equal(t, "0", after[name], name)
	}
	_, rss := after["used_memory_rss"]
	assert.Equal(t, runtime.GOOS == "linux", rss, "RSS is read where the platform reports it")
}

// TestINFOCPUIsInRedisFormat: processor time as Redis writes it, seconds with
// six decimals.
func TestINFOCPUIsInRedisFormat(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("no getrusage")
	}
	e := newTestEngine(t, Options{})
	fields := infoFields(t, runOn(t, e, "INFO", "cpu").(string))
	for _, name := range []string{"used_cpu_sys", "used_cpu_user", "used_cpu_sys_children", "used_cpu_user_children"} {
		assert.Regexp(t, `^\d+\.\d{6}$`, fields[name], name)
	}
}

// TestINFOFeaturesKeelLacksReportRedisIdleValues: Redis's values for blocking
// commands, Pub/Sub, tracking, scripts, forks and snapshots not in use, which
// are true of Keel.
func TestINFOFeaturesKeelLacksReportRedisIdleValues(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	fields := infoFields(t, runOn(t, e, "INFO", "persistence", "stats").(string))
	for name, want := range map[string]string{
		"loading": "0", "async_loading": "0", "rdb_bgsave_in_progress": "0", "rdb_last_bgsave_status": "ok", "rdb_saves": "0",
		"evicted_clients": "0", "evicted_scripts": "0", "pubsub_channels": "0", "pubsub_patterns": "0",
		"pubsubshard_channels": "0", "latest_fork_usec": "0", "total_forks": "0", "tracking_total_keys": "0",
	} {
		assert.Equal(t, want, fields[name], name)
	}
}

// TestINFOClientsSectionHoldsTheCommandBudget: the command budget's lines are
// part of the Clients section, under its header and before its blank line,
// with or without the connection hook.
func TestINFOClientsSectionHoldsTheCommandBudget(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	e.SetCommandAllocations(&CommandAllocationBudget{Limit: 1 << 20})
	info := runOn(t, e, "INFO", "clients").(string)
	assert.Regexp(t, `^# Clients\r\ncommand_allocation_limit_bytes:1048576\r\n(command_allocation_[a-z_]+:\d+\r\n)+\r\n$`, info)
	e.SetClientBuffers(func() ClientBufferStats { return ClientBufferStats{Connected: 2} })
	info = runOn(t, e, "INFO", "clients").(string)
	assert.Regexp(t, `^# Clients\r\nconnected_clients:2\r\n(.+\r\n)+command_allocation_refusals:0\r\n\r\n$`, info)
}
