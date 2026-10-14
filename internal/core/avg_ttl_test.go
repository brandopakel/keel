package core

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// keyspaceLine is INFO keyspace's db0 line, or "" when there is none.
func keyspaceLine(t *testing.T, e *Engine) string {
	t.Helper()
	text, ok := runOn(t, e, "INFO", "keyspace").(string)
	require.True(t, ok)
	return infoFields(t, text)["db0"]
}

// TestAvgTTLFoldsAsRedisDoes: the first estimate is taken as it is, and each
// later one moves it 2% a round toward the new mean, as Redis's
// activeExpireCycle computes it, truncated as Redis's long long is.
func TestAvgTTLFoldsAsRedisDoes(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	e.foldAvgTTL(0, 0, 0)
	assert.Zero(t, e.avgTTL, "no sample, no estimate")
	e.foldAvgTTL(3000, 3, 1)
	assert.Equal(t, int64(1000), e.avgTTL, "the first estimate as it is")
	e.foldAvgTTL(2000, 1, 1)
	assert.Equal(t, int64(1020), e.avgTTL, "2000 + (1000-2000)*0.98")
	e.avgTTL = 1000
	e.foldAvgTTL(4000, 2, 3)
	assert.Equal(t, int64(1058), e.avgTTL, "2000 + (1000-2000)*0.941192, truncated")
}

// TestKeyspaceLineIsRedis: no line for an empty keyspace, as Redis writes
// none; once keys are held, Redis's four fields, avg_ttl from the expiry
// cycle's sample of the keys that carry a TTL; and avg_ttl back to 0 when no
// key does, and on FLUSHDB.
func TestKeyspaceLineIsRedis(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.Empty(t, keyspaceLine(t, e), "an empty keyspace has no line")
	runOn(t, e, "SET", "plain", "v")
	assert.Equal(t, "keys=1,expires=0,avg_ttl=0,subexpiry=0", keyspaceLine(t, e))

	for _, k := range []string{"a", "b", "c"} {
		runOn(t, e, "SET", k, "v", "EX", "3600")
	}
	e.ExpireCycle()
	fields := map[string]string{}
	for _, part := range strings.Split(keyspaceLine(t, e), ",") {
		k, v, _ := strings.Cut(part, "=")
		fields[k] = v
	}
	assert.Equal(t, "4", fields["keys"])
	assert.Equal(t, "3", fields["expires"])
	avg, err := strconv.ParseInt(fields["avg_ttl"], 10, 64)
	require.NoError(t, err)
	assert.True(t, avg > 3_590_000 && avg <= 3_600_000, "avg_ttl %d ms, an hour less what has passed", avg)

	runOn(t, e, "PERSIST", "a")
	runOn(t, e, "PERSIST", "b")
	runOn(t, e, "PERSIST", "c")
	e.ExpireCycle()
	assert.Contains(t, keyspaceLine(t, e), "avg_ttl=0", "no key carries a TTL")

	runOn(t, e, "SET", "d", "v", "EX", "60")
	e.ExpireCycle()
	assert.NotContains(t, keyspaceLine(t, e), "avg_ttl=0")
	runOn(t, e, "FLUSHDB")
	assert.Empty(t, keyspaceLine(t, e), "flushed")
	assert.Zero(t, e.avgTTL, "FLUSHDB zeroes it, as Redis's emptyData does")
}

// TestNoTTLIsSampledWhileExpiryIsSuspended: while expiry is suspended, as a
// replay of the log suspends it, active expiry notes no time left.
func TestNoTTLIsSampledWhileExpiryIsSuspended(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "SET", "a", "v", "EX", "3600")
	e.space.SuspendExpiry = true
	e.dictStore.ActiveExpire(10)
	e.space.SuspendExpiry = false
	_, n := e.space.TakeTTLSamples()
	assert.Zero(t, n)
	e.dictStore.ActiveExpire(10)
	_, n = e.space.TakeTTLSamples()
	assert.Equal(t, uint64(1), n)
}
