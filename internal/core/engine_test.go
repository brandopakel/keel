package core

import (
	"io"
	"math"
	"strings"
	"sync"
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
	res, _ := Decode(rawOn(t, e, name, args...))
	return res
}

// rawOn is on without the decoding.
func rawOn(t *testing.T, e *Engine, name string, args ...string) []byte {
	t.Helper()
	var w replyWriter
	require.NoError(t, e.evalAndResponse(&Command{Cmd: name, Args: args}, &w))
	return w.b
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
// Not parallel: the reply's protocol and the log are still package state
// until the rest of step 2.2 and step 2.3 of the plan move them.
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

// TestEnginesShareNoCommandScope: what an engine holds for the command running
// on it is its own - the allocation budget its commands reserve from, the
// reply ceiling its EXEC lowers, and the name GEOSEARCH was sent as - and a
// command on another engine sees none of it, whether that command runs after
// it, in the middle of its EXEC, or at the same time.
func TestEnginesShareNoCommandScope(t *testing.T) {
	ResetStores()
	a := newEngine(data_structure.NewSpace(engineLimits(math.MaxInt)))
	b := newEngine(data_structure.NewSpace(engineLimits(math.MaxInt)))
	value := strings.Repeat("v", 64<<10)
	for _, e := range []*Engine{a, b} {
		require.Equal(t, MaxReplyBytes, e.replyCeiling)
		require.Nil(t, e.commandAllocations)
		require.Equal(t, "OK", on(t, e, "SET", "large", value))
		require.Equal(t, int64(1), on(t, e, "GEOADD", "g", "0", "0", "here"))
	}

	// A budget too small for the reply, on a alone, refuses a's read and
	// none of b's; then b's own budget holds b's reservation and not a's.
	a.commandAllocations = &CommandAllocationBudget{Limit: 16 << 10}
	assert.Equal(t, allocationPressure, rawOn(t, a, "GET", "large"))
	assert.Equal(t, uint64(1), a.commandAllocations.Refusals)
	assert.Equal(t, value, on(t, b, "GET", "large"), "b reserves from no budget")
	b.commandAllocations = &CommandAllocationBudget{Limit: 1 << 20}
	assert.Equal(t, value, on(t, b, "GET", "large"))
	assert.Positive(t, b.commandAllocations.ReplyReserved)
	assert.Zero(t, a.commandAllocations.Reserved, "b's reservation is not a's")
	assert.Equal(t, uint64(1), a.commandAllocations.Refusals)
	assert.Nil(t, defaultEngine.commandAllocations, "neither budget is the default engine's")
	a.commandAllocations, b.commandAllocations = nil, nil

	// An EXEC on a lowers a's reply ceiling for the commands after each
	// reply. A command run on b in the middle of it has the whole limit.
	var during []byte
	conn := &midTransaction{answer: func() {
		assert.Less(t, a.replyCeiling, MaxReplyBytes, "a is inside its EXEC")
		assert.Equal(t, MaxReplyBytes, b.replyCeiling)
		during = rawOn(t, b, "GET", "large")
	}}
	var tx *Transaction
	for _, cmd := range [][]string{{"MULTI"}, {"GET", "large"}, {"AUTH", "b"}, {"GET", "large"}} {
		var w replyWriter
		var err error
		tx, err = a.transact(tx, &Command{Cmd: cmd[0], Args: cmd[1:]}, &w, conn)
		require.NoError(t, err)
	}
	var w replyWriter
	_, err := a.transact(tx, &Command{Cmd: "EXEC"}, &w, conn)
	require.NoError(t, err)
	replies, _ := Decode(w.b)
	assert.Equal(t, []interface{}{value, "OK", value}, replies)
	got, _ := Decode(during)
	assert.Equal(t, value, got)
	assert.Equal(t, MaxReplyBytes, a.replyCeiling, "EXEC restores a's ceiling")

	// Side by side, as two engines will run once each has a lock of its own
	// (plan phase 3). Each goroutine sets its engine's scope the way
	// evalAndResponse and an event-loop run do and calls the handlers
	// directly, since the log is package state until step 2.3. Under -race,
	// command-scope state the engines shared would fail here.
	a.commandAllocations = &CommandAllocationBudget{Limit: 16 << 10}
	b.commandAllocations = &CommandAllocationBudget{Limit: 1 << 20}
	geosearch := []string{"g", "BYRADIUS", "1", "km", "COUNT", "1"}
	var wg sync.WaitGroup
	for _, side := range []struct {
		e          *Engine
		name       string
		refused    bool
		allocation *CommandAllocationBudget
	}{{a, "geosearch", true, a.commandAllocations}, {b, "GeoSearch", false, b.commandAllocations}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				side.e.runningName = side.name
				refusal := string(side.e.cmdGEOSEARCH(geosearch))
				side.allocation.Begin(0)
				reply := side.e.cmdGET([]string{"large"})
				side.allocation.End()
				if !assert.True(t, strings.HasSuffix(refusal, " for "+side.name+"\r\n"), refusal) ||
					!assert.Equal(t, side.refused, string(reply) == string(allocationPressure), side.name) {
					return
				}
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, uint64(200), a.commandAllocations.Refusals)
	assert.Zero(t, b.commandAllocations.Refusals)
}

// midTransaction is a transport whose own command, run in its place inside an
// EXEC, calls answer.
type midTransaction struct{ answer func() }

func (m *midTransaction) RESP3() bool { return false }
func (m *midTransaction) AnswerConnection(_ *Command, w io.ReadWriter) {
	m.answer()
	w.Write([]byte("+OK\r\n"))
}
