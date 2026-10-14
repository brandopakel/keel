package core

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
}

// TestNetworkRatesAreKilobytesASecond: a rate sample is bytes a second over
// the time since the last, and INFO reports the mean of the last 16 in
// kilobytes a second, as Redis's getInstantaneousMetric / 1024.
func TestNetworkRatesAreKilobytesASecond(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	e.SetClientBuffers(func() ClientBufferStats { return ClientBufferStats{} })
	start := time.Unix(2000, 0)
	e.totals.ops.sample(0, start)
	e.totals.netIn.sample(0, start)
	e.totals.netIn.sample(1024*16, start.Add(100*time.Millisecond))
	assert.Equal(t, "10.00", statsOn(t, e)["instantaneous_input_kbps"], "160 KiB/s, one sample of 16, as Redis averages them")
}

// TestSampleCommandRateSamplesTheTransportsBytes: the cron's sample takes the
// transport's byte totals into the rates, replication's in the totals and on
// their own, in the same periods as ops; the next sample a period on turns
// what came in between into a rate INFO reports.
func TestSampleCommandRateSamplesTheTransportsBytes(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	stats := ClientBufferStats{NetInputBytes: 1000, NetOutputBytes: 5000, NetReplInputBytes: 30, NetReplOutputBytes: 70}
	e.SetClientBuffers(func() ClientBufferStats { return stats })
	e.SampleCommandRate()
	tt := &e.totals
	assert.Equal(t, [4]uint64{1030, 5070, 30, 70},
		[4]uint64{tt.netIn.lastValue, tt.netOut.lastValue, tt.replIn.lastValue, tt.replOut.lastValue},
		"the first sample sets each rate's base from the transport's totals")
	assert.True(t, tt.netIn.lastAt.Equal(tt.ops.lastAt), "in ops's period")

	// A period on: the samples are moved back rather than slept through.
	for _, m := range []*instantaneousMetric{&tt.ops, &tt.netIn, &tt.netOut, &tt.replIn, &tt.replOut} {
		m.lastAt = m.lastAt.Add(-instantaneousPeriod)
	}
	stats.NetInputBytes += 1024 * 16
	e.SampleCommandRate()
	kbps, err := strconv.ParseFloat(statsOn(t, e)["instantaneous_input_kbps"], 64)
	require.NoError(t, err)
	// 16 KiB over at least one period of 100 ms is at most 160 KiB/s, one
	// sample of 16: at most 10, and short of that only by the time the test
	// took between the two samples.
	assert.True(t, kbps > 5 && kbps <= 10, "instantaneous_input_kbps %v", kbps)
	assert.Equal(t, "0.00", statsOn(t, e)["instantaneous_input_repl_kbps"], "no replication input came in")
}
