package core

import (
	"math"
	"testing"
	"time"

	"github.com/brandopakel/keel/internal/config"
	"github.com/brandopakel/keel/internal/data_structure"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// on runs a command on e the way EvalAndResponse runs one on the default
// engine, and decodes the reply.
func on(t *testing.T, e *Engine, name string, args ...string) interface{} {
	t.Helper()
	var w replyWriter
	require.NoError(t, e.evalAndResponse(&Command{Cmd: name, Args: args}, &w))
	res, _ := Decode(w.b)
	return res
}

// engineLimits are the server's limits from config, with a key bound of its
// own.
func engineLimits(maxKeys int) data_structure.Limits {
	return data_structure.Limits{
		EvictStrategy: config.LRU, KeyNumberLimit: maxKeys, LRUSamples: config.LRUSamples,
		LFULogFactor: config.LFULogFactor, LFUDecayPeriod: config.LFUDecayPeriod, LCSMaxCells: config.LCSMaxCells,
	}
}

// TestEnginesShareNoKeys: each engine is a keyspace of its own. A key written
// on one is not on the other, whatever its type; deleting, flushing, a bound
// that evicts and the expiry cycle each act on one engine only; and neither
// touches the default engine the server runs on.
//
// Not parallel: the command scope and the log are still package state until
// steps 2.2 and 2.3 of the plan move them.
func TestEnginesShareNoKeys(t *testing.T) {
	ResetStores()
	a := newEngine(data_structure.NewSpace(engineLimits(math.MaxInt)))
	b := newEngine(data_structure.NewSpace(engineLimits(3)))

	// Every family, on a; the same names hold other types on b.
	for _, cmd := range [][]string{
		{"SET", "s", "a"}, {"HSET", "h", "f", "a"}, {"RPUSH", "l", "a"}, {"SADD", "set", "a"},
		{"ZADD", "z", "1", "a"}, {"PFADD", "hll", "a"}, {"BF.ADD", "bf", "a"}, {"CF.ADD", "cf", "a"},
		{"CMS.INITBYDIM", "cms", "10", "2"}, {"MORRIS.INITBYDIM", "m", "8", "2"},
	} {
		var w replyWriter
		require.NoError(t, a.evalAndResponse(&Command{Cmd: cmd[0], Args: cmd[1:]}, &w))
		require.NotEqual(t, byte('-'), w.b[0], "%v answered %q", cmd, w.b)
	}
	assert.Equal(t, int64(10), on(t, a, "DBSIZE"))
	assert.Equal(t, int64(0), on(t, b, "DBSIZE"))
	for _, key := range []string{"s", "h", "l", "set", "z", "hll", "bf", "cf", "cms", "m"} {
		assert.Equal(t, "none", on(t, b, "TYPE", key), key)
	}
	require.Equal(t, int64(1), on(t, b, "SADD", "s", "b"), "a name a holds as a string is free on b")
	assert.Equal(t, "set", on(t, b, "TYPE", "s"))
	assert.Equal(t, "a", on(t, a, "GET", "s"))
	assert.Equal(t, int64(1), on(t, b, "DEL", "s"))
	assert.Equal(t, "string", on(t, a, "TYPE", "s"), "a DEL on b leaves a's key")

	// b holds three keys at most, and evicts its own to stay inside that.
	for _, key := range []string{"k1", "k2", "k3", "k4", "k5"} {
		require.Equal(t, "OK", on(t, b, "SET", key, "b"))
	}
	assert.Equal(t, int64(3), on(t, b, "DBSIZE"))
	assert.Equal(t, uint64(2), b.space.Evicted())
	assert.Equal(t, int64(10), on(t, a, "DBSIZE"), "b's bound evicts nothing of a's")
	assert.Zero(t, a.space.Evicted())

	// Each engine's expiry cycle reaps its own keys and counts them itself.
	require.Equal(t, "OK", on(t, a, "SET", "brief", "a", "PX", "1"))
	time.Sleep(5 * time.Millisecond)
	require.Equal(t, 1, a.ExpireCycle())
	assert.Equal(t, uint64(1), a.ExpiredKeys())
	assert.Zero(t, b.ExpiredKeys())
	assert.Zero(t, b.ExpireCycle())

	assert.Equal(t, "OK", on(t, a, "FLUSHDB"))
	assert.Equal(t, int64(0), on(t, a, "DBSIZE"))
	assert.Equal(t, int64(3), on(t, b, "DBSIZE"), "a FLUSHDB on a leaves b's keys")

	assert.Zero(t, defaultEngine.space.TotalKeys(), "neither engine wrote to the default one")
	assert.Zero(t, defaultEngine.ExpiredKeys())
}
