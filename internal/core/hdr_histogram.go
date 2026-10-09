package core

import (
	"math"
	"math/bits"
)

// latencyHistogram is the part of HdrHistogram that Redis keeps a command's
// latencies in (deps/hdr_histogram/hdr_histogram.c in Redis 8.10.1), ported
// line for line so that its percentiles and LATENCY HISTOGRAM's buckets are
// the ones Redis reports for the same times: values from 1 ns to 1 s, to two
// significant figures. Recording a value costs a few shifts and an add.
type latencyHistogram struct {
	counts     []int64
	totalCount int64
}

// Redis's LATENCY_HISTOGRAM_MIN_VALUE, _MAX_VALUE and _PRECISION, and the
// bucket layout hdr_calculate_bucket_config derives from them.
const (
	latencyMin       = 1             // nanoseconds
	latencyMax       = 1_000_000_000 // nanoseconds, one second
	latencyPrecision = 2             // significant figures

	// 2 * 10^2 = 200 needs 8 bits: sub-buckets of 256, half of them 128.
	hdrSubBucketHalfCountMagnitude = 7
	hdrSubBucketCount              = 1 << (hdrSubBucketHalfCountMagnitude + 1)
	hdrSubBucketHalfCount          = hdrSubBucketCount / 2
	// log2 of the lowest discernible value, 1.
	hdrUnitMagnitude = 0
	hdrSubBucketMask = (hdrSubBucketCount - 1) << hdrUnitMagnitude
)

// hdrBucketCount is buckets_needed_to_cover_value for latencyMax, and
// hdrCountsLen the counts it needs.
var hdrBucketCount = func() int32 {
	smallestUntrackable := int64(hdrSubBucketCount) << hdrUnitMagnitude
	needed := int32(1)
	for smallestUntrackable <= latencyMax {
		if smallestUntrackable > math.MaxInt64/2 {
			return needed + 1
		}
		smallestUntrackable <<= 1
		needed++
	}
	return needed
}()

var hdrCountsLen = (hdrBucketCount + 1) * (hdrSubBucketCount / 2)

func newLatencyHistogram() *latencyHistogram {
	return &latencyHistogram{counts: make([]int64, hdrCountsLen)}
}

func hdrBucketIndex(value int64) int32 {
	pow2ceiling := int32(64 - bits.LeadingZeros64(uint64(value|hdrSubBucketMask)))
	return pow2ceiling - hdrUnitMagnitude - (hdrSubBucketHalfCountMagnitude + 1)
}

func hdrSubBucketIndex(value int64, bucket int32) int32 {
	return int32(value >> (bucket + hdrUnitMagnitude))
}

func hdrCountsIndex(bucket, sub int32) int32 {
	return (bucket+1)<<hdrSubBucketHalfCountMagnitude + (sub - hdrSubBucketHalfCount)
}

func hdrValueFromIndex(bucket, sub int32) int64 {
	return int64(sub) << (bucket + hdrUnitMagnitude)
}

func hdrValueAtIndex(index int32) int64 {
	bucket := (index >> hdrSubBucketHalfCountMagnitude) - 1
	sub := (index & (hdrSubBucketHalfCount - 1)) + hdrSubBucketHalfCount
	if bucket < 0 {
		sub -= hdrSubBucketHalfCount
		bucket = 0
	}
	return hdrValueFromIndex(bucket, sub)
}

func hdrSizeOfEquivalentRange(bucket, sub int32) int64 {
	adjusted := bucket
	if sub >= hdrSubBucketCount {
		adjusted = bucket + 1
	}
	return int64(1) << (hdrUnitMagnitude + adjusted)
}

func hdrLowestEquivalent(value int64) int64 {
	bucket := hdrBucketIndex(value)
	return hdrValueFromIndex(bucket, hdrSubBucketIndex(value, bucket))
}

func hdrHighestEquivalent(value int64) int64 {
	bucket := hdrBucketIndex(value)
	sub := hdrSubBucketIndex(value, bucket)
	return hdrValueFromIndex(bucket, sub) + hdrSizeOfEquivalentRange(bucket, sub) - 1
}

// record is updateCommandLatencyHistogram's: the value clamped to the range,
// then hdr_record_value.
func (h *latencyHistogram) record(value int64) {
	value = min(max(value, latencyMin), latencyMax)
	bucket := hdrBucketIndex(value)
	index := hdrCountsIndex(bucket, hdrSubBucketIndex(value, bucket))
	if index < 0 || index >= int32(len(h.counts)) {
		return
	}
	h.counts[index]++
	h.totalCount++
}

// valueAtPercentile is hdr_value_at_percentile.
func (h *latencyHistogram) valueAtPercentile(percentile float64) int64 {
	requested := min(percentile, 100.0)
	countAt := int64(requested/100*float64(h.totalCount) + 0.5)
	countAt = max(countAt, 1)
	var value int64
	var cumulative int64
	for i, c := range h.counts {
		cumulative += c
		if cumulative >= countAt {
			value = hdrValueAtIndex(int32(i))
			break
		}
	}
	if percentile == 0 {
		return hdrLowestEquivalent(value)
	}
	return hdrHighestEquivalent(value)
}

// logBucket is one step of hdr_iter_log: the highest value equivalent to
// the bucket the step stopped in, and the count of every value up to it.
type logBucket struct {
	highestEquivalent int64
	cumulativeCount   int64
}

// hdrIter is hdr_iter's basic part: where it stands in the counts, and the
// bucket it stands on.
type hdrIter struct {
	h                   *latencyHistogram
	index               int32
	cumulative          int64
	value, highestEquiv int64
}

// moveNext is move_next.
func (it *hdrIter) moveNext() bool {
	it.index++
	if it.index >= int32(len(it.h.counts)) {
		return false
	}
	it.cumulative += it.h.counts[it.index]
	it.value = hdrValueAtIndex(it.index)
	bucket := hdrBucketIndex(it.value)
	sub := hdrSubBucketIndex(it.value, bucket)
	it.highestEquiv = hdrValueFromIndex(bucket, sub) + hdrSizeOfEquivalentRange(bucket, sub) - 1
	return true
}

// nextValueAbove is next_value_greater_than_reporting_level_upper_bound,
// which peeks one index on even past the last.
func (it *hdrIter) nextValueAbove(bound int64) bool {
	if it.index >= int32(len(it.h.counts)) {
		return false
	}
	return hdrValueAtIndex(it.index+1) > bound
}

// logBuckets is hdr_iter_log_init(h, first, 2) walked to its end with
// log_iter_next, as LATENCY HISTOGRAM walks it: reporting levels from first,
// doubling. Each step reports the bucket it stopped in, the one that reached
// the level, with that bucket's highest equivalent value and the count of
// everything up to and including it.
func (h *latencyHistogram) logBuckets(first int64) []logBucket {
	it := hdrIter{h: h, index: -1}
	level, levelLowest := first, hdrLowestEquivalent(first)
	var out []logBucket
	for it.cumulative < h.totalCount || it.nextValueAbove(levelLowest) {
		for {
			if it.value >= levelLowest {
				level *= 2
				levelLowest = hdrLowestEquivalent(level)
				break
			}
			if !it.moveNext() {
				break
			}
		}
		out = append(out, logBucket{it.highestEquiv, it.cumulative})
	}
	return out
}
