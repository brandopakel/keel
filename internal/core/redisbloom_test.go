package core

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The number readers are RedisBloom's. Each case below is one Redis 8.10.1
// with RedisBloom 8.10.1 was sent, as a BF.RESERVE error rate or capacity, by
// scripts/redisbloom-parity.py: ok is whether it read the text as a number at
// all (a number out of range is still a number).

func TestRedisDoubleReadsAsRedisDoes(t *testing.T) {
	for _, c := range []struct {
		text string
		ok   bool
		want float64
	}{
		{"0.01", true, 0.01}, {"1e-2", true, 0.01}, {"1E-2", true, 0.01}, {"+0.1", true, 0.1}, {".5", true, 0.5},
		{"5.", true, 5}, {"0.", true, 0}, {"00.1", true, 0.1}, {"1e+0", true, 1}, {"-0", true, 0}, {"1.e-1", true, 0.1},
		{"inf", true, math.Inf(1)}, {"+inf", true, math.Inf(1)}, {"-inf", true, math.Inf(-1)},
		{"infinity", true, math.Inf(1)}, {"Infinity", true, math.Inf(1)}, {"INF", true, math.Inf(1)},
		{"0x1p-3", true, 0.125}, {"0X1P-3", true, 0.125}, {"0x10", true, 16}, {"0x.8", true, 0.5},
		{"0x1.8", true, 1.5}, {"0x0p0", true, 0}, {"-0x1p-3", true, -0.125}, {"0x8p-4", true, 0.5},
		{"0x1P+1", true, 2}, {"4.9e-324", true, 5e-324}, {"1e-320", true, 1e-320},
		{"2.2250738585072014e-308", true, 2.2250738585072014e-308},
		{"abc", false, 0}, {"", false, 0}, {" 0.1", false, 0}, {"0.1 ", false, 0}, {"\t0.1", false, 0},
		{"0.1\n", false, 0}, {"nan", false, 0}, {"NaN", false, 0}, {"-nan", false, 0}, {"nan(1)", false, 0},
		{"1e-400", false, 0}, {"0x1p-1100", false, 0}, {"1e400", false, 0}, {"0.01abc", false, 0},
		{"0,01", false, 0}, {"0.0.1", false, 0}, {"1e", false, 0}, {"e1", false, 0}, {"--0.1", false, 0},
		{"0.01\x00", false, 0}, {"0x1p", false, 0}, {"0x", false, 0}, {"0x_1p0", false, 0}, {"1_0", false, 0},
	} {
		got, ok := redisDouble(c.text)
		assert.Equal(t, c.ok, ok, "%q", c.text)
		if c.ok {
			assert.Equal(t, c.want, got, "%q", c.text)
		}
	}
}

func TestRedisIntegerReadsAsRedisDoes(t *testing.T) {
	for _, c := range []struct {
		text string
		ok   bool
		want int64
	}{
		{"0", true, 0}, {"1", true, 1}, {"-1", true, -1}, {"100", true, 100},
		{"9223372036854775807", true, math.MaxInt64}, {"-9223372036854775808", true, math.MinInt64},
		{"abc", false, 0}, {"", false, 0}, {" 5", false, 0}, {"5 ", false, 0}, {"-0", false, 0},
		{"007", false, 0}, {"0x10", false, 0}, {"+5", false, 0}, {"1.0", false, 0}, {"1e3", false, 0},
		{"9223372036854775808", false, 0}, {"-9223372036854775809", false, 0}, {"18446744073709551616", false, 0},
		{"-", false, 0}, {"000000000000000000001", false, 0},
	} {
		got, ok := redisInteger(c.text)
		assert.Equal(t, c.ok, ok, "%q", c.text)
		assert.Equal(t, c.want, got, "%q", c.text)
	}
}

// TestFilterCommandsFromAnEarlierPrimaryApply: a replica applies a primary's
// log prefix as a log is replayed, so what the earlier build logged - and
// RedisBloom refuses - applies there too, with no error to stop the replica.
func TestFilterCommandsFromAnEarlierPrimaryApply(t *testing.T) {
	ResetStores()
	t.Cleanup(func() { replicaApplying = false; ResetStores() })
	replicaApplying = true
	for _, args := range legacyBloomLog {
		reply := rawReply(t, args[0], args[1:]...)
		require.NotEqual(t, byte('-'), reply[0], "%q answered %q", args, reply)
	}
	replicaApplying = false
	_, each := dumpImages(t, legacyBloomKeys)
	want := loadRedisBloomPersistence(t)
	for key, image := range want.LegacyEach {
		assert.Equal(t, image, each[key], "%s", key)
	}
	// A client sending the same commands is held to RedisBloom's rules.
	assert.Equal(t, "-Not found\r\n", string(rawReply(t, "CF.DEL", "missing", "x")))
	assert.Equal(t, "-Bad capacity\r\n", string(rawReply(t, "CF.RESERVE", "fresh", "007")))
	assert.Equal(t, "-ERR expansion must be in the range [0, 32768]\r\n",
		string(rawReply(t, "BF.RESERVE", "fresh", "0.01", "10", "EXPANSION", "100000")))
}
