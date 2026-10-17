package core

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const oomReply = "-OOM command not allowed when used memory > 'maxmemory'.\r\n"

// TestNoEvictionRefusesWhatWouldGrowTheKeyspace: under noeviction the write
// that crosses maxmemory runs, as Redis checks memory before a command rather
// than after; then a DENYOOM command is refused, as Redis's processCommand
// refuses it, while reads and deletes run, and once a delete brings memory
// back under the bound writes run again.
func TestNoEvictionRefusesWhatWouldGrowTheKeyspace(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{MaxMemory: 1, Eviction: EvictNone})
	s := newSession(t, e)
	require.Equal(t, "+OK\r\n", s.send("SET", "a", "v"))
	assert.Equal(t, oomReply, s.send("SET", "b", "v"))
	assert.Equal(t, oomReply, s.send("APPEND", "a", "x"))
	assert.Equal(t, "$1\r\nv\r\n", s.send("GET", "a"))
	assert.Equal(t, ":1\r\n", s.send("DEL", "a"))
	assert.Equal(t, "+OK\r\n", s.send("SET", "b", "v"))

	fields := infoFields(t, runOn(t, e, "INFO", "stats", "errorstats", "commandstats").(string))
	assert.Equal(t, "count=2", fields["errorstat_OOM"])
	assert.Contains(t, fields["cmdstat_set"], "rejected_calls=1,")
	assert.Contains(t, fields["cmdstat_append"], "rejected_calls=1,")
	assert.Equal(t, "0", fields["evicted_keys"])
	assert.Contains(t, s.send("CONFIG", "GET", "maxmemory-policy"), "noeviction")
}

// TestVolatilePolicyRefusesOnceNoKeyCarriesATTL: a volatile policy evicts the
// keys that carry a TTL, and once none is left to evict the keyspace stays
// over its bound and DENYOOM commands are refused.
func TestVolatilePolicyRefusesOnceNoKeyCarriesATTL(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{MaxMemory: 1, Eviction: EvictVolatileLRU})
	s := newSession(t, e)
	require.Equal(t, "+OK\r\n", s.send("SET", "volatile", "v", "EX", "3600"))
	assert.Equal(t, ":0\r\n", s.send("EXISTS", "volatile"), "evicted, the one key with a TTL")
	require.Equal(t, "+OK\r\n", s.send("SET", "kept", "v"))
	assert.Equal(t, oomReply, s.send("SET", "refused", "v"))
	assert.Equal(t, ":1\r\n", s.send("EXISTS", "kept"))
	assert.Equal(t, "1", infoFields(t, runOn(t, e, "INFO", "stats").(string))["evicted_keys"])
}

// TestOOMRefusalInATransaction: a DENYOOM command refused as it is queued
// spoils the transaction, as any refusal at queueing does; and EXEC, when
// memory has gone over since its commands were queued, is refused as a whole
// if it holds one, with Redis's EXECABORT and the reason, and runs if it
// holds none.
func TestOOMRefusalInATransaction(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{MaxMemory: 1, Eviction: EvictNone})
	s := newSession(t, e)
	require.Equal(t, "+OK\r\n", s.send("SET", "a", "v"))
	s.send("MULTI")
	assert.Equal(t, oomReply, s.send("SET", "b", "v"))
	assert.Equal(t, "+QUEUED\r\n", s.send("GET", "a"))
	assert.Equal(t, "-EXECABORT Transaction discarded because of previous errors.\r\n", s.send("EXEC"))

	require.Equal(t, ":1\r\n", s.send("DEL", "a"))
	s.send("MULTI")
	require.Equal(t, "+QUEUED\r\n", s.send("SET", "b", "v"))
	require.Equal(t, "+QUEUED\r\n", s.send("SET", "c", "v"))
	reconfigure(t, e, func(o *Options) { o.MaxMemory = 1 })
	runOn(t, e, "SET", "over", "v")
	assert.Equal(t, "-EXECABORT Transaction discarded because of: OOM command not allowed when used memory > 'maxmemory'.\r\n",
		s.send("EXEC"))
	assert.Equal(t, ":0\r\n", s.send("EXISTS", "b"), "none of it ran")

	s.send("MULTI")
	s.send("GET", "over")
	assert.Equal(t, "*1\r\n$1\r\nv\r\n", s.send("EXEC"), "a transaction of reads runs")
}

// TestAReplicaRefusesNothingForMemory: a replica ignores maxmemory, as
// Redis's does by default (replica-ignore-maxmemory), so the primary's writes
// apply whatever the bound.
func TestAReplicaRefusesNothingForMemory(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{MaxMemory: 1, Eviction: EvictNone})
	reconfigure(t, e, func(o *Options) { o.ReplicaOf = "primary.test:6379" })
	e.replicaApplying = true
	t.Cleanup(func() { e.replicaApplying = false })
	for _, key := range []string{"a", "b", "c"} {
		assert.Equal(t, "OK", runOn(t, e, "SET", key, "v"))
	}
	e.replicaApplying = false
	assert.False(t, e.oomRefuses("SET"), "and refuses a client nothing for memory either")
}

// TestAllkeysPoliciesNeverRefuse: under an allkeys policy every write evicts
// until the keyspace is back inside its bounds, so nothing is refused.
func TestAllkeysPoliciesNeverRefuse(t *testing.T) {
	t.Parallel()
	for _, policy := range []EvictionPolicy{EvictLRU, EvictLFU, EvictRandom} {
		e := newTestEngine(t, Options{MaxMemory: 1, Eviction: policy})
		for _, key := range []string{"a", "b", "c"} {
			assert.Equal(t, "OK", runOn(t, e, "SET", key, "v"), "policy %d", policy)
		}
	}
}

// TestEvictionPoliciesHaveRedisNames: each policy goes by its Redis name,
// which INFO and CONFIG GET report, and a name Keel has no policy for is
// refused.
func TestEvictionPoliciesHaveRedisNames(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"volatile-lru", "volatile-lfu", "volatile-random", "volatile-ttl", "allkeys-lru",
		"allkeys-lfu", "allkeys-random", "noeviction"} {
		policy, ok := EvictionPolicyNamed(name)
		require.True(t, ok, name)
		assert.Equal(t, name, evictionPolicyName(policy))
		e := newTestEngine(t, Options{Eviction: policy})
		assert.Equal(t, name, infoFields(t, runOn(t, e, "INFO", "memory").(string))["maxmemory_policy"])
	}
	policy, ok := EvictionPolicyNamed("VOLATILE-TTL")
	assert.True(t, ok)
	assert.Equal(t, EvictVolatileTTL, policy)
	for _, name := range []string{"volatile-lrm", "allkeys-lrm", "lru", ""} {
		_, ok := EvictionPolicyNamed(name)
		assert.False(t, ok, name)
	}
}

// TestConfigSetChoosesEveryPolicyLive: CONFIG SET maxmemory-policy takes each
// of Keel's policies by Redis's name and holds the engine to it at once, as
// CONFIG GET reports; volatile-lrm and allkeys-lrm, which Keel does not have
// yet, are refused in Redis's words, naming the ones it has.
func TestConfigSetChoosesEveryPolicyLive(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{MaxMemory: 1})
	s := newSession(t, e)
	for _, name := range []string{"volatile-lru", "volatile-lfu", "volatile-random", "volatile-ttl", "allkeys-lfu",
		"allkeys-random", "noeviction", "allkeys-lru"} {
		require.Equal(t, "+OK\r\n", s.send("CONFIG", "SET", "maxmemory-policy", name), name)
		policy, _ := EvictionPolicyNamed(name)
		assert.Equal(t, policy, e.space.EvictionPolicy(), name)
		assert.Contains(t, s.send("CONFIG", "GET", "maxmemory-policy"), "$"+itoa(len(name))+"\r\n"+name+"\r\n", name)
	}
	assert.Equal(t, "-ERR CONFIG SET failed (possibly related to argument 'maxmemory-policy') - argument(s) must be one of "+
		"the following: volatile-lru, volatile-lfu, volatile-random, volatile-ttl, allkeys-lru, allkeys-lfu, allkeys-random, "+
		"noeviction\r\n", s.send("CONFIG", "SET", "maxmemory-policy", "volatile-lrm"))

	// Live: a keyspace over its bound refuses writes the moment the policy is
	// noeviction, and evicts for them again under allkeys-lru.
	require.Equal(t, "+OK\r\n", s.send("CONFIG", "SET", "maxmemory-policy", "noeviction"))
	require.Equal(t, "+OK\r\n", s.send("SET", "a", "v"))
	assert.Equal(t, oomReply, s.send("SET", "b", "v"))
	require.Equal(t, "+OK\r\n", s.send("CONFIG", "SET", "maxmemory-policy", "allkeys-lru"))
	assert.Equal(t, "+OK\r\n", s.send("SET", "b", "v"))
}

func itoa(n int) string { return strconv.Itoa(n) }

// TestEvictionExceededTimeIsRedis: the time the keyspace is over its bound
// with nothing to evict starts at the first command that finds it so and ends
// at the first that finds it back inside, as Redis's performEvictions keeps it
// before each command; INFO reports the time so far and the total, in
// milliseconds; a switch to an allkeys policy ends it; and CONFIG RESETSTAT
// zeroes both, as resetServerStats does.
func TestEvictionExceededTimeIsRedis(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{MaxMemory: 1, Eviction: EvictNone})
	s := newSession(t, e)
	exceeded := func() (total, current int) {
		fields := infoFields(t, runOn(t, e, "INFO", "stats").(string))
		total, _ = strconv.Atoi(fields["total_eviction_exceeded_time"])
		current, _ = strconv.Atoi(fields["current_eviction_exceeded_time"])
		return total, current
	}
	require.Equal(t, "+OK\r\n", s.send("SET", "a", "v"))
	total, current := exceeded()
	assert.Zero(t, total+current, "INFO is the first command to find it over")
	// Waited past what is asserted, so that clock resolution and the
	// milliseconds INFO truncates to leave a margin.
	waitPast(40)
	total, current = exceeded()
	assert.GreaterOrEqual(t, current, 30)
	assert.Equal(t, total, current)

	require.Equal(t, ":1\r\n", s.send("DEL", "a"))
	s.send("PING") // the first command to find it back inside
	waitPast(10)
	total, current = exceeded()
	assert.Zero(t, current)
	assert.GreaterOrEqual(t, total, 30, "the time over is kept")

	require.Equal(t, "+OK\r\n", s.send("SET", "b", "v"))
	s.send("PING")
	require.NotZero(t, e.evictionExceededSince)
	require.Equal(t, "+OK\r\n", s.send("CONFIG", "SET", "maxmemory-policy", "allkeys-lru"))
	assert.Zero(t, e.evictionExceededSince, "an allkeys policy ends it")
	waitPast(10)
	total, current = exceeded()
	assert.Zero(t, current, "and nothing starts it again")
	assert.GreaterOrEqual(t, total, 30, "the time it ended is kept")

	runOn(t, e, "CONFIG", "RESETSTAT")
	total, current = exceeded()
	assert.Zero(t, total+current)
}

// TestEvictionExceededTimeStaysZeroWhereNothingIsMeasured: an allkeys policy
// never leaves the keyspace over its bound, and a replica measures nothing,
// as Redis's returns before performEvictions does.
func TestEvictionExceededTimeStaysZeroWhereNothingIsMeasured(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{MaxMemory: 1})
	runOn(t, e, "SET", "a", "v")
	runOn(t, e, "SET", "b", "v")
	assert.Zero(t, e.evictionExceededSince+e.evictionExceededNanos)

	r := newTestEngine(t, Options{MaxMemory: 1, Eviction: EvictNone})
	reconfigure(t, r, func(o *Options) { o.ReplicaOf = "primary.test:6379" })
	r.replicaApplying = true
	runOn(t, r, "SET", "a", "v")
	r.replicaApplying = false
	assert.False(t, r.oomRefuses("GET"))
	assert.Zero(t, r.evictionExceededSince+r.evictionExceededNanos)
}
