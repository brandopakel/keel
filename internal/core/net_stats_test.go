package core

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestNetworkStatsAreRedisFields: the transport's byte counts under Redis's
// names, its totals including replication's as Redis's do, the rates in
// kilobytes a second sampled as Redis samples them, and the reads and writes
// it made; none of them for an engine no transport drives.
func TestNetworkStatsAreRedisFields(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.NotContains(t, statsOn(t, e), "total_net_input_bytes", "no transport, no network")

	stats := ClientBufferStats{NetInputBytes: 1000, NetOutputBytes: 5000, NetReplInputBytes: 30, NetReplOutputBytes: 70,
		ReadsProcessed: 7, WritesProcessed: 9}
	e.SetClientBuffers(func() ClientBufferStats { return stats })
	fields := statsOn(t, e)
	assert.Equal(t, "1030", fields["total_net_input_bytes"], "replication's included")
	assert.Equal(t, "5070", fields["total_net_output_bytes"])
	assert.Equal(t, "30", fields["total_net_repl_input_bytes"])
	assert.Equal(t, "70", fields["total_net_repl_output_bytes"])
	assert.Equal(t, "7", fields["total_reads_processed"])
	assert.Equal(t, "9", fields["total_writes_processed"])

	start := time.Unix(2000, 0)
	e.totals.ops.sample(0, start)
	e.totals.netIn.sample(0, start)
	e.totals.netIn.sample(1024*16, start.Add(100*time.Millisecond))
	assert.Equal(t, "10.00", statsOn(t, e)["instantaneous_input_kbps"], "160 KiB/s, one sample of 16, as Redis averages them")
}
