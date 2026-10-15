package core

import (
	"os"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/brandopakel/keel/internal/constant"
)

// statsFields is INFO stats on e, field by field.
func statsFields(t *testing.T, e *Engine) map[string]string {
	t.Helper()
	return infoFields(t, runOn(t, e, "INFO", "stats").(string))
}

// TestExpiredKeysCountsEveryKeyATTLRemoved: expired_keys counts a key a read
// found expired as well as one the cycle removed, as Redis's stat_expiredkeys
// does, and expired_keys_active only the cycle's.
func TestExpiredKeysCountsEveryKeyATTLRemoved(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "SET", "read", "v", "PX", "5")
	runOn(t, e, "SET", "cycle", "v", "PX", "5")
	runOn(t, e, "HSET", "hash", "f", "v")
	runOn(t, e, "PEXPIRE", "hash", "5")
	runOn(t, e, "SET", "alive", "v", "EX", "1000")
	waitPast(30)

	assert.Equal(t, constant.RespNil, rawReplyOn(t, e, "GET", "read"))
	assert.EqualValues(t, 0, runOn(t, e, "EXISTS", "hash"))
	for i := 0; i < 50 && e.KeysWithExpiry() > 1; i++ {
		e.ExpireCycle()
	}
	require.Equal(t, 1, e.KeysWithExpiry(), "the cycle removed the key nobody read")

	fields := statsFields(t, e)
	assert.Equal(t, "3", fields["expired_keys"])
	assert.Equal(t, "1", fields["expired_keys_active"])
	assert.Equal(t, "0", fields["expired_subkeys"], "Keel has no field expiry yet")
	assert.Equal(t, "0", fields["expired_subkeys_active"])
}

// TestSetWithAnInstantPastStoresNothing: SET with an EXAT or PXAT that has
// passed stores nothing, as Redis's setGenericCommand: it deletes the key if
// there is one, recorded as DEL, counts the key as expired, and answers as if
// it had stored it. NX and XX decide first.
func TestSetWithAnInstantPastStoresNothing(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := withAOFOn(t, e, func() {
		runOn(t, e, "SET", "str", "old")
		runOn(t, e, "SET", "nx", "kept")
		runOn(t, e, "HSET", "hash", "f", "v")
		runOn(t, e, "CONFIG", "RESETSTAT")

		assert.Equal(t, "OK", runOn(t, e, "SET", "absent", "v", "PXAT", "1"))
		assert.Equal(t, "old", runOn(t, e, "SET", "str", "new", "PXAT", "1", "GET"))
		assert.Equal(t, "OK", runOn(t, e, "SET", "hash", "v", "EXAT", "1"))
		assert.Equal(t, constant.RespNil, rawReplyOn(t, e, "SET", "nx", "v", "NX", "PXAT", "1"))
		assert.Equal(t, constant.RespNil, rawReplyOn(t, e, "SET", "missing", "v", "XX", "PXAT", "1"))

		assert.Equal(t, 1, e.space.TotalKeys(), "only nx, which NX kept")
		assert.Equal(t, "kept", runOn(t, e, "GET", "nx"))
		fields := statsFields(t, e)
		assert.Equal(t, "3", fields["expired_keys"], "absent, str and hash")
		assert.Equal(t, "0", fields["expired_keys_active"])
	})
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	log := string(data)
	assert.NotContains(t, log, "absent", "a key never stored is not recorded")
	assert.NotContains(t, log, "new")
	assert.Contains(t, log, "*2\r\n$3\r\nDEL\r\n$3\r\nstr\r\n")
	assert.Contains(t, log, "*2\r\n$3\r\nDEL\r\n$4\r\nhash\r\n")
	restartOn(t, e, path)
	assert.Equal(t, 1, e.space.TotalKeys())
}

// TestASetWithAnInstantPastReplaysAsWritten: a log an earlier build wrote,
// whose SET carries an instant since passed, replays as written - Redis's
// checkAlreadyExpired does not delete while loading - and the pass after the
// replay removes the key, as the cycle would, counting it.
func TestASetWithAnInstantPastReplaysAsWritten(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := withAOFOn(t, e, func() {
		runOn(t, e, "SET", "k", "v", "PX", "5")
		runOn(t, e, "SET", "alive", "v")
	})
	waitPast(30)
	restartOn(t, e, path)
	assert.Equal(t, 1, e.space.TotalKeys())
	fields := statsFields(t, e)
	assert.Equal(t, "1", fields["expired_keys"])
	assert.Equal(t, "1", fields["expired_keys_active"])
}

// TestExpireCycleKeepsRedisStatisticsOfItself: expired_stale_perc is
// Redis's running average of what each cycle found expired of what it
// sampled, a cycle's sample weighing 5%, which a cycle with no TTL to sample
// folds in as nothing stale; expired_time_cap_reached_count counts the cycles
// that stopped at their cap with more to do; expire_cycle_cpu_milliseconds is
// the time the cycles took. CONFIG RESETSTAT clears all three.
func TestExpireCycleKeepsRedisStatisticsOfItself(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	for i := 0; i < 5000; i++ {
		runOn(t, e, "SET", "d"+strconv.Itoa(i), "v", "PX", "5")
	}
	waitPast(30)

	// Every key sampled has expired, so each cycle draws all its rounds.
	held := e.options.WithDefaults()
	require.Equal(t, held.ActiveExpireSamples*held.ActiveExpireRounds, e.ExpireCycle())
	fields := statsFields(t, e)
	assert.Equal(t, "5.00", fields["expired_stale_perc"], "the first sample, all stale, at 5%")
	assert.Equal(t, "1", fields["expired_time_cap_reached_count"])
	require.Equal(t, held.ActiveExpireSamples*held.ActiveExpireRounds, e.ExpireCycle())
	fields = statsFields(t, e)
	assert.Equal(t, "9.75", fields["expired_stale_perc"], "1 at 5%, and 5% at 95%")
	assert.Equal(t, "2", fields["expired_time_cap_reached_count"])
	assert.Positive(t, e.expireCycleUsec, "removing hundreds of keys takes some microseconds")
	assert.Equal(t, strconv.FormatUint(e.expireCycleUsec/1000, 10), fields["expire_cycle_cpu_milliseconds"])

	require.Equal(t, "OK", runOn(t, e, "FLUSHDB"))
	e.ExpireCycle()
	assert.Equal(t, "9.26", statsFields(t, e)["expired_stale_perc"], "nothing to sample is nothing stale")

	runOn(t, e, "CONFIG", "RESETSTAT")
	fields = statsFields(t, e)
	for _, name := range []string{"expired_keys", "expired_keys_active", "expired_time_cap_reached_count", "expire_cycle_cpu_milliseconds"} {
		assert.Equal(t, "0", fields[name], name)
	}
	assert.Equal(t, "0.00", fields["expired_stale_perc"])
	assert.Zero(t, e.expireCycleUsec)
}

// TestScanRemovesTheExpiredKeysItWalks: SCAN removes, and counts, each key it
// walks whose TTL has passed and which matches its pattern, whatever its
// type, as Redis's scanCallback expires a key before it looks at the type. A
// key the pattern does not match is left for something else to find.
func TestScanRemovesTheExpiredKeysItWalks(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{ActiveExpireSamples: Off})
	runOn(t, e, "SET", "xs", "v", "PX", "5")
	runOn(t, e, "HSET", "xh", "f", "v")
	runOn(t, e, "PEXPIRE", "xh", "5")
	runOn(t, e, "SET", "ys", "v", "PX", "5")
	runOn(t, e, "SET", "xalive", "v")
	waitPast(30)

	var found []interface{}
	for cursor := "0"; ; {
		reply := runOn(t, e, "SCAN", cursor, "MATCH", "x*", "TYPE", "string", "COUNT", "100").([]interface{})
		found = append(found, reply[1].([]interface{})...)
		if cursor = reply[0].(string); cursor == "0" {
			break
		}
	}
	assert.Equal(t, []interface{}{"xalive"}, found)
	assert.Equal(t, 2, e.space.TotalKeys(), "xs and xh are removed, the hash too; ys did not match")
	fields := statsFields(t, e)
	assert.Equal(t, "2", fields["expired_keys"])
	assert.Equal(t, "0", fields["expired_keys_active"])
}

// TestMemoryUsageReportsAKeyWhoseTTLHasPassed: MEMORY USAGE finds its key as
// Redis 8's dbFind does, so a key whose TTL has passed is reported as it is
// held, not removed, and not counted, until something that looks it up
// removes it.
func TestMemoryUsageReportsAKeyWhoseTTLHasPassed(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{ActiveExpireSamples: Off})
	runOn(t, e, "SET", "k", "v", "PX", "5")
	held := runOn(t, e, "MEMORY", "USAGE", "k")
	waitPast(30)

	assert.Equal(t, held, runOn(t, e, "MEMORY", "USAGE", "k"))
	assert.Equal(t, 1, e.space.TotalKeys())
	assert.Equal(t, "0", statsFields(t, e)["expired_keys"])
	assert.Equal(t, constant.RespNil, rawReplyOn(t, e, "GET", "k"))
	assert.Equal(t, constant.RespNil, rawReplyOn(t, e, "MEMORY", "USAGE", "k"))
	assert.Equal(t, "1", statsFields(t, e)["expired_keys"])
}
