package core

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLatencyHistogramMatchesRedisHdrHistogram: the same values give the
// same percentiles and the same LATENCY HISTOGRAM buckets as Redis 8.10.1's
// HdrHistogram. testdata/hdr_histogram_redis.json is that code's output:
// deps/hdr_histogram/hdr_histogram.c compiled with a harness that records
// each value as updateCommandLatencyHistogram does and prints p50, p99,
// p99.9, p0 and p100 as INFO latencystats writes them, then the buckets as
// fillCommandCDF writes them.
func TestLatencyHistogramMatchesRedisHdrHistogram(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("testdata/hdr_histogram_redis.json")
	require.NoError(t, err)
	var sets []struct {
		Name        string
		Values      []int64
		Percentiles []string
		Buckets     [][2]int64
	}
	require.NoError(t, json.Unmarshal(raw, &sets))
	require.NotEmpty(t, sets)
	for _, set := range sets {
		h := newLatencyHistogram()
		for _, v := range set.Values {
			h.record(v)
		}
		var percentiles []string
		for _, p := range []float64{50, 99, 99.9, 0, 100} {
			percentiles = append(percentiles, fmt.Sprintf("%.3f", float64(h.valueAtPercentile(p))/1000.0))
		}
		assert.Equal(t, set.Percentiles, percentiles, set.Name)
		var buckets [][2]int64
		previous := int64(0)
		for _, b := range h.logBuckets(1024) {
			if b.cumulativeCount > previous {
				buckets = append(buckets, [2]int64{b.highestEquivalent / 1000, b.cumulativeCount})
			}
			previous = b.cumulativeCount
		}
		if len(set.Buckets) == 0 {
			assert.Empty(t, buckets, set.Name)
		} else {
			assert.Equal(t, set.Buckets, buckets, set.Name)
		}
	}
}
