package core

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brandopakel/keel/internal/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// on runs a command on e the way a connection would, and decodes the reply.
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

// TestEnginesShareNoKeys: each engine is a keyspace of its own. A key written
// on one is not on the other, whatever its type; deleting, flushing, a bound
// that evicts and the expiry cycle each act on one engine only; and neither
// touches a third engine that stands by.
func TestEnginesShareNoKeys(t *testing.T) {
	t.Parallel()
	bystander := newTestEngine(t, Options{})
	a := newTestEngine(t, Options{})
	b := newTestEngine(t, Options{MaxKeys: 3})

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

	assert.Zero(t, bystander.space.TotalKeys(), "neither engine wrote to the one standing by")
	assert.Zero(t, bystander.ExpiredKeys())
}

// TestEnginesShareNoCommandScope: what an engine holds for the command running
// on it is its own - the protocol its reply is framed in, the allocation
// budget its commands reserve from, the reply ceiling and eviction suspension
// of its EXEC, and the name GEOSEARCH was sent as - and a command on another
// engine sees none of it, whether that command runs after it, at the same
// time, or in the middle of its EXEC; and a third engine standing by holds
// none of it.
func TestEnginesShareNoCommandScope(t *testing.T) {
	t.Parallel()
	const bound = 8
	bystander := newTestEngine(t, Options{MaxKeys: bound})
	a := newTestEngine(t, Options{MaxKeys: bound})
	b := newTestEngine(t, Options{MaxKeys: bound})
	value := strings.Repeat("v", 64<<10)
	for _, e := range []*Engine{a, b} {
		require.Equal(t, MaxReplyBytes, e.replyCeiling)
		require.Nil(t, e.commandAllocations)
		require.Equal(t, "OK", on(t, e, "SET", "large", value))
		require.Equal(t, int64(1), on(t, e, "GEOADD", "g", "0", "0", "here"))
		require.Equal(t, int64(1), on(t, e, "HSET", "h", "f", "v"))
	}

	// A RESP3 command on a frames its own reply, and nothing of b's.
	var w3 replyWriter
	require.NoError(t, a.evalAndResponse(&Command{Cmd: "HGETALL", Args: []string{"h"}, RESP3: true}, &w3))
	assert.Equal(t, "%1\r\n$1\r\nf\r\n$1\r\nv\r\n", string(w3.b))
	assert.Equal(t, "*2\r\n$1\r\nf\r\n$1\r\nv\r\n", string(rawOn(t, b, "HGETALL", "h")))
	assert.False(t, a.replyRESP3, "a's command is over")

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
	assert.Nil(t, bystander.commandAllocations, "neither budget is the bystander's")

	// Each engine's GEOSEARCH error names the command as it was sent there.
	geosearch := []string{"g", "BYRADIUS", "1", "km", "COUNT", "1"}
	for _, sent := range []struct {
		e    *Engine
		name string
	}{{a, "geosearch"}, {b, "GeoSearch"}, {a, "GEOsearch"}} {
		var w replyWriter
		require.NoError(t, sent.e.evalAndResponse(&Command{Cmd: "GEOSEARCH", Name: sent.name, Args: geosearch}, &w))
		assert.True(t, strings.HasSuffix(string(w.b), " for "+sent.name+"\r\n"), string(w.b))
		assert.Equal(t, sent.name, sent.e.runningName)
	}
	assert.Equal(t, "GeoSearch", b.runningName, "a's commands leave b's name")

	// Side by side, as two engines will run once each has a lock of its own
	// (plan phase 3), each through evalAndResponse with a log of its own open:
	// a in RESP3 with a budget too small for the large value, b in RESP2 with
	// room for it, each counting its runs in a key only its own log records.
	// The transport begins and ends each run on its budget, as here. Under
	// -race, command-scope or log state the engines shared would fail here.
	a.commandAllocations = &CommandAllocationBudget{Limit: 16 << 10}
	b.commandAllocations = &CommandAllocationBudget{Limit: 1 << 20}
	logs := t.TempDir()
	require.NoError(t, a.OpenAOF(filepath.Join(logs, "a.aof")))
	require.NoError(t, b.OpenAOF(filepath.Join(logs, "b.aof")))
	var wg sync.WaitGroup
	for _, side := range []struct {
		e             *Engine
		resp3         bool
		name          string
		hash, missing string
		refused       bool
		allocation    *CommandAllocationBudget
	}{
		{a, true, "geosearch", "%1\r\n$1\r\nf\r\n$1\r\nv\r\n", "_\r\n", true, a.commandAllocations},
		{b, false, "GeoSearch", "*2\r\n$1\r\nf\r\n$1\r\nv\r\n", "$-1\r\n", false, b.commandAllocations},
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			run := func(name string, args ...string) string {
				var w replyWriter
				cmd := &Command{Cmd: strings.ToUpper(name), Name: name, Args: args, RESP3: side.resp3}
				assert.NoError(t, side.e.evalAndResponse(cmd, &w))
				return string(w.b)
			}
			for i := range 200 {
				side.allocation.Begin(0)
				refusal := run(side.name, geosearch...)
				hash := run("HGETALL", "h")
				missing := run("GET", "missing")
				large := run("GET", "large")
				count := run("INCR", "runs:"+side.name)
				side.allocation.End()
				if !assert.NoError(t, side.e.FlushAOF()) ||
					!assert.True(t, strings.HasSuffix(refusal, " for "+side.name+"\r\n"), refusal) ||
					!assert.Equal(t, side.hash, hash) || !assert.Equal(t, side.missing, missing) ||
					!assert.Equal(t, side.refused, large == string(allocationPressure), side.name) ||
					!assert.Equal(t, ":"+strconv.Itoa(i+1)+"\r\n", count) {
					return
				}
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, uint64(200), a.commandAllocations.Refusals)
	assert.Zero(t, b.commandAllocations.Refusals)
	for _, side := range []struct {
		e           *Engine
		own, others string
	}{{a, "runs:geosearch", "runs:GeoSearch"}, {b, "runs:GeoSearch", "runs:geosearch"}} {
		path := side.e.aof.path
		require.NoError(t, side.e.CloseAOF())
		log, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, 200, strings.Count(string(log), string(appendCommand(nil, "INCR", side.own))), path)
		assert.NotContains(t, string(log), side.others, "%s holds only its engine's writes", path)
	}
	a.commandAllocations, b.commandAllocations = nil, nil

	// An EXEC on a lowers a's reply ceiling for the commands after each reply,
	// and holds a's evictions until it is over, so a passes its bound
	// meanwhile. A command run on b in the middle of it has the whole output
	// limit, and b evicts at once when it passes its own bound. Last, because
	// what eviction removes is the LRU's choice.
	// Each engine holds the same keys, and this many writes take one a key
	// past its bound.
	past := bound + 1 - a.space.TotalKeys()
	writes := func(prefix string) [][]string {
		var out [][]string
		for i := range past {
			out = append(out, []string{"SET", prefix + strconv.Itoa(i), "v"})
		}
		return out
	}
	var during []byte
	conn := &midTransaction{t: t, answer: func() {
		assert.True(t, a.space.SuspendEviction, "a is inside its EXEC")
		assert.Less(t, a.replyCeiling, MaxReplyBytes)
		assert.Greater(t, a.space.TotalKeys(), bound, "a holds its evictions")
		assert.Zero(t, a.space.Evicted())
		assert.False(t, b.space.SuspendEviction)
		assert.Equal(t, MaxReplyBytes, b.replyCeiling)
		during = rawOn(t, b, "GET", "large")
		for _, cmd := range writes("b") {
			require.Equal(t, "OK", on(t, b, cmd[0], cmd[1:]...))
		}
		assert.Equal(t, bound, b.space.TotalKeys(), "b evicts as it goes")
		assert.Equal(t, uint64(1), b.space.Evicted())
	}}
	queued := append(append([][]string{{"MULTI"}, {"GET", "large"}}, writes("a")...), []string{"AUTH", "b"}, []string{"GET", "large"})
	var tx *Transaction
	for _, cmd := range queued {
		var w replyWriter
		var err error
		tx, err = a.transact(tx, &Command{Cmd: cmd[0], Args: cmd[1:]}, &w, conn)
		require.NoError(t, err)
	}
	var w replyWriter
	_, err := a.transact(tx, &Command{Cmd: "EXEC"}, &w, conn)
	require.NoError(t, err)
	replies, _ := Decode(w.b)
	want := []interface{}{value}
	for range past {
		want = append(want, "OK")
	}
	assert.Equal(t, append(want, "OK", value), replies)
	got, _ := Decode(during)
	assert.Equal(t, value, got)
	assert.Equal(t, MaxReplyBytes, a.replyCeiling, "EXEC restores a's ceiling")
	assert.False(t, a.space.SuspendEviction)
	assert.Equal(t, bound, a.space.TotalKeys(), "a evicts once its EXEC is over")
	assert.Equal(t, uint64(1), a.space.Evicted())
	assert.Equal(t, uint64(1), b.space.Evicted())
}

// TestEnginesShareNoLog: each engine records its writes in a log of its own,
// through I/O of its own, and a log replays into the engine that reads it and
// no other. One engine's records, transaction frames, reaped keys and failed
// disk stay its own; and two engines with a log open on each run side by side
// through evalAndResponse, one appending on the worker and one synchronously,
// each log replaying to its own engine's keyspace. A third engine, standing
// by, is left as it was, and its rewrite is left alone by another engine's
// writes. Under -race, log state the engines shared would fail here.
func TestEnginesShareNoLog(t *testing.T) {
	t.Parallel()
	bystander := newTestEngine(t, Options{})
	encoded, written, synced, ready := bystander.AOFPositions()
	bystanderPositions := [4]uint64{encoded, written, synced, ready}
	dir := t.TempDir()
	a := newTestEngine(t, Options{Fsync: FsyncAlways})
	b := newTestEngine(t, Options{Fsync: FsyncAlways})
	var aWrites, bSyncs atomic.Int64
	a.aofWrite = func(f *os.File, body []byte) (int, error) { aWrites.Add(1); return f.Write(body) }
	b.aofSync = func(f *os.File) error { bSyncs.Add(1); return f.Sync() }
	require.NoError(t, a.OpenAOF(filepath.Join(dir, "a.aof")))
	require.NoError(t, b.OpenAOF(filepath.Join(dir, "b.aof")))
	logOf := func(e *Engine) string {
		require.NoError(t, e.FlushAOF())
		body, err := os.ReadFile(e.aof.path)
		require.NoError(t, err)
		return string(body)
	}
	records := func(commands ...[]string) string {
		var out []byte
		for _, c := range commands {
			out = appendCommand(out, c...)
		}
		return string(out)
	}

	// The same name, as two types, on two engines: each log has its own
	// engine's record, written and synced through that engine's I/O.
	require.Equal(t, "OK", on(t, a, "SET", "k", "a"))
	require.Equal(t, int64(1), on(t, b, "HSET", "k", "f", "b"))
	assert.Equal(t, records([]string{"SET", "k", "a"}), logOf(a))
	assert.Equal(t, records([]string{"HSET", "k", "f", "b"}), logOf(b))
	assert.Equal(t, int64(1), aWrites.Load(), "a's writes go through a's I/O")
	assert.Equal(t, int64(1), bSyncs.Load(), "b's syncs go through b's I/O")

	// A transaction on a is framed in a's log; a key b reaps is deleted in
	// b's log; a's removal hook is a's.
	var tx *Transaction
	for _, cmd := range [][]string{{"MULTI"}, {"SET", "t", "a"}, {"INCR", "n"}, {"EXEC"}} {
		var w replyWriter
		var err error
		tx, err = a.transact(tx, &Command{Cmd: cmd[0], Args: cmd[1:]}, &w, nil)
		require.NoError(t, err)
	}
	require.Equal(t, "OK", on(t, b, "SET", "brief", "b", "PX", "1"))
	time.Sleep(5 * time.Millisecond)
	assert.Equal(t, "$-1\r\n", string(rawOn(t, b, "GET", "brief")), "b reaped it")
	assert.Equal(t, records([]string{"SET", "k", "a"}, []string{"MULTI"}, []string{"SET", "t", "a"},
		[]string{"INCR", "n"}, []string{"EXEC"}), logOf(a))
	bLog := logOf(b)
	assert.True(t, strings.HasSuffix(bLog, records([]string{"DEL", "brief"})), "b reaped its own key: %q", bLog)
	assert.NotContains(t, bLog, "MULTI")

	// a's disk fails: a's log latches the failure and reports it, and b goes
	// on writing to its own.
	diskErr := errors.New("a's disk is full")
	a.aofWrite = func(*os.File, []byte) (int, error) { return 0, diskErr }
	require.Equal(t, "OK", on(t, a, "SET", "lost", "a"))
	require.ErrorIs(t, a.FlushAOF(), diskErr)
	require.Equal(t, "OK", on(t, b, "SET", "kept", "b"))
	require.NoError(t, b.FlushAOF())
	assert.Contains(t, on(t, a, "INFO", "persistence"), "aof_last_write_status:err")
	assert.Contains(t, on(t, b, "INFO", "persistence"), "aof_last_write_status:ok")
	assert.Contains(t, on(t, a, "INFO", "persistence"), "aof_write_errors:1\r\n", "a counts its failed write")
	assert.Contains(t, on(t, b, "INFO", "persistence"), "aof_write_errors:0\r\n", "and b does not")
	require.ErrorIs(t, a.CloseAOF(), diskErr)
	a.aofWrite = writeLog
	a.resetStores() // a's next log starts from an empty keyspace
	require.NoError(t, a.OpenAOF(filepath.Join(dir, "a2.aof")))

	// Side by side: a appends on its worker, b synchronously, each writing
	// the same names with values of its own.
	for _, e := range []*Engine{a, b} {
		reconfigure(t, e, func(o *Options) { o.Fsync = FsyncEverySec })
	}
	var wg sync.WaitGroup
	for _, side := range []struct {
		e      *Engine
		name   string
		worker bool
	}{{a, "a", true}, {b, "b", false}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e := side.e
			flush := func() bool {
				if !side.worker {
					return assert.NoError(t, e.FlushAOF())
				}
				for {
					ready, err := e.FlushAOFAsync(nil)
					if !assert.NoError(t, err) {
						return false
					}
					if ready {
						return true
					}
					time.Sleep(10 * time.Microsecond)
				}
			}
			do := func(parts ...string) bool {
				var w replyWriter
				err := e.evalAndResponse(&Command{Cmd: parts[0], Args: parts[1:]}, &w)
				return assert.NoError(t, err) && assert.NotEqual(t, byte('-'), w.b[0], "%q: %q", parts, w.b)
			}
			for i := range 300 {
				v := side.name + ":" + strconv.Itoa(i)
				ok := do("SET", "s:"+strconv.Itoa(i%40), v) && do("INCR", "count") &&
					do("HSET", "h", "f"+strconv.Itoa(i%9), v) && do("RPUSH", "l", v) && do("SADD", "set", v) &&
					do("SET", "brief:"+strconv.Itoa(i), v, "PX", "1")
				if ok && i%25 == 0 {
					var tx *Transaction
					for _, cmd := range [][]string{{"MULTI"}, {"SET", "tx", v}, {"LPOP", "l"}, {"EXEC"}} {
						var w replyWriter
						var err error
						if tx, err = e.transact(tx, &Command{Cmd: cmd[0], Args: cmd[1:]}, &w, nil); !assert.NoError(t, err) {
							return
						}
					}
					e.ExpireCycle()
				}
				if !ok || !flush() {
					return
				}
			}
		}()
	}
	wg.Wait()

	// Each log replays to its own engine's keyspace, and holds nothing of the
	// other's. The last brief:* keys are let pass their millisecond first:
	// the keyspace below reads one still within it as present, while the
	// replay, a moment later, reaps it.
	time.Sleep(5 * time.Millisecond)
	never := goldenWindow{start: -1, end: -1 - int64(24*time.Hour/time.Millisecond)}
	for _, side := range []struct {
		e            *Engine
		own, other   string
		firstLogName string
	}{{a, "a:", "b:", "a2.aof"}, {b, "b:", "a:", "b.aof"}} {
		path := side.e.aof.path
		assert.Equal(t, filepath.Join(dir, side.firstLogName), path)
		require.NoError(t, side.e.CloseAOF())
		want := engineState(t, side.e, never)
		require.Contains(t, string(want), side.own)
		replayed := newEngine(Options{})
		_, err := replayed.LoadAOF(path)
		require.NoError(t, err)
		assert.Equal(t, string(want), string(engineState(t, replayed, never)), "%s replays to its engine", path)
		log, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.NotContains(t, string(log), side.other, "%s holds only its engine's writes", path)
	}
	assert.Nil(t, bystander.aof.file, "neither opened the bystander's log")
	encoded, written, synced, ready = bystander.AOFPositions()
	assert.Equal(t, bystanderPositions, [4]uint64{encoded, written, synced, ready}, "neither moved the bystander's offsets")
	assert.Zero(t, bystander.space.TotalKeys())

	// While the bystander rewrites its log, another engine's writes,
	// removals, flushes and close leave that rewrite alone.
	require.NoError(t, bystander.OpenAOF(filepath.Join(dir, "bystander.aof")))
	for i := range 10 {
		require.Equal(t, "OK", on(t, bystander, "SET", "s:"+strconv.Itoa(i), "bystander"))
	}
	require.NoError(t, bystander.StartRewrite())
	c := newTestEngine(t, Options{})
	require.NoError(t, c.OpenAOF(filepath.Join(dir, "c.aof")))
	require.Equal(t, "OK", on(t, c, "SET", "s:1", "c"))
	require.Equal(t, int64(1), on(t, c, "SADD", "set", "only"))
	on(t, c, "SPOP", "set")
	require.Equal(t, "OK", on(t, c, "SET", "brief", "c", "PX", "1"))
	time.Sleep(5 * time.Millisecond)
	assert.Equal(t, "$-1\r\n", string(rawOn(t, c, "GET", "brief")))
	require.Equal(t, "OK", on(t, c, "FLUSHDB"))
	require.NoError(t, c.FlushAOF())
	require.NoError(t, c.CloseAOF())
	assert.True(t, bystander.RewriteActive(), "c's close leaves the bystander's rewrite running")
	assert.Empty(t, bystander.rewrite.dirty, "c's keys are not the bystander's dirty keys")
	assert.Zero(t, bystander.rewrite.pos, "c's flushes do not advance the bystander's rewrite")
}

// TestEnginesShareNoRewrite: a rewrite walks its own engine's keyspace and
// replaces its own engine's log, through that engine's I/O and handoff, and
// wakes the loop that drives that engine. A failing disk under one engine's
// rewrite, a key ceiling it hits, the keys written while it runs, a rewrite
// its EXEC schedules, a directory sync its rename leaves pending, what its
// rewrites come to, the wait they earn and the I/O they are timed by are that
// engine's alone, whether the engines rewrite one after the other or side by
// side; and a third engine standing by is left with what its own rewrites came
// to.
func TestEnginesShareNoRewrite(t *testing.T) {
	t.Parallel()
	bystander := newTestEngine(t, Options{})
	bystanderOutcome, bystanderRewrites := bystander.rewriteOutcome, bystander.aof.rewrites
	dir := t.TempDir()
	a := newTestEngine(t, Options{})
	b := newTestEngine(t, Options{})
	var aWoken, bWoken atomic.Int64
	a.SetRewriteWaker(func() { aWoken.Add(1) })
	b.SetRewriteWaker(func() { bWoken.Add(1) })
	require.NoError(t, a.OpenAOF(filepath.Join(dir, "a.aof")))
	require.NoError(t, b.OpenAOF(filepath.Join(dir, "b.aof")))
	for i := range 50 {
		for _, side := range []struct {
			e    *Engine
			name string
		}{{a, "a"}, {b, "b"}} {
			v := side.name + ":" + strconv.Itoa(i)
			require.Equal(t, "OK", on(t, side.e, "SET", "s:"+strconv.Itoa(i%10), v))
			on(t, side.e, "RPUSH", "l", v)
		}
	}
	require.NoError(t, a.FlushAOF())
	require.NoError(t, b.FlushAOF())
	read := func(e *Engine) string {
		body, err := os.ReadFile(e.aof.path)
		require.NoError(t, err)
		return string(body)
	}
	drive := func(e *Engine) {
		for i := 0; e.RewriteActive(); i++ {
			require.NoError(t, e.FlushAOF())
			require.Less(t, i, 100000, "the rewrite did not end")
			time.Sleep(20 * time.Microsecond)
		}
	}
	never := goldenWindow{start: -1, end: -1 - int64(24*time.Hour/time.Millisecond)}
	replays := func(e *Engine) {
		t.Helper()
		replayed := newEngine(Options{})
		_, err := replayed.LoadAOF(e.aof.path)
		require.NoError(t, err)
		assert.Equal(t, string(engineState(t, e, never)), string(engineState(t, replayed, never)))
	}

	// a's rewrite: a key written on a while it runs is a's dirty key, and one
	// written on b is not; it replaces a's log and leaves b's as it was.
	bBefore := read(b)
	require.NoError(t, a.StartRewrite())
	assert.False(t, b.RewriteActive())
	require.Equal(t, "OK", on(t, a, "SET", "during", "a"))
	require.Equal(t, "OK", on(t, b, "SET", "during", "b"))
	assert.Contains(t, a.rewrite.dirty, "during")
	require.NoError(t, b.FlushAOF())
	drive(a)
	assert.Equal(t, 1, a.aof.rewrites)
	assert.Zero(t, b.aof.rewrites)
	assert.Equal(t, bBefore+string(appendCommand(nil, "SET", "during", "b")), read(b), "b's log is only b's writes")
	assert.NotContains(t, read(a), "b:")
	assert.Positive(t, aWoken.Load(), "a's workers wake a's loop")
	assert.Zero(t, bWoken.Load(), "and not b's")
	replays(a)

	// b's rewrite on a failing disk, and then a's key ceiling: each engine's
	// own hooks, and neither touches the other's log.
	diskErr := errors.New("b's disk is full")
	b.rewriteFileWrite = func(*os.File, []byte) (int, error) { return 0, diskErr }
	aBefore := read(a)
	require.NoError(t, b.StartRewrite())
	drive(b)
	assert.Zero(t, b.aof.rewrites, "b's rewrite failed")
	assert.True(t, sameFile(t, b.aof.file, b.aof.path), "b's old log is still b's log")
	_, err := os.Stat(b.aof.path + ".rewrite")
	assert.True(t, os.IsNotExist(err), "b's failed rewrite removed its file")
	b.rewriteFileWrite = writeLog
	a.keyCountForRewrite = func() int { return RewriteKeyCeiling + 1 }
	require.Error(t, a.StartRewrite())
	assert.False(t, a.RewriteActive())
	require.NoError(t, b.StartRewrite(), "a's ceiling is not b's")
	drive(b)
	assert.Equal(t, 1, b.aof.rewrites)
	assert.Equal(t, aBefore, read(a), "b's rewrites leave a's log as it was")
	replays(a)
	replays(b)
	a.keyCountForRewrite = a.space.TotalKeys

	field := func(e *Engine, name string) string {
		for _, line := range strings.Split(string(rawOn(t, e, "INFO", "persistence")), "\r\n") {
			if value, ok := strings.CutPrefix(line, name+":"); ok {
				return value
			}
		}
		t.Fatalf("INFO persistence has no %s", name)
		return ""
	}

	// A BGREWRITEAOF inside a's EXEC is scheduled on a: b's flush starts
	// nothing, and a's own starts it.
	var tx *Transaction
	for _, cmd := range []string{"MULTI", "BGREWRITEAOF", "EXEC"} {
		var w replyWriter
		var err error
		tx, err = a.transact(tx, &Command{Cmd: cmd}, &w, nil)
		require.NoError(t, err)
	}
	assert.Equal(t, "1", field(a, "aof_rewrite_scheduled"))
	assert.Equal(t, "0", field(b, "aof_rewrite_scheduled"))
	require.NoError(t, b.FlushAOF())
	assert.False(t, b.RewriteActive(), "b's flush does not start a's scheduled rewrite")
	assert.Equal(t, "1", field(a, "aof_rewrite_scheduled"))
	require.NoError(t, a.FlushAOF())
	assert.True(t, a.RewriteActive(), "a's own flush does")
	drive(a)
	assert.Equal(t, 2, a.aof.rewrites)

	// b's rename leaves the directory sync after it pending on b alone: a's
	// syncs and a's reopened log neither retry it nor clear it, and b's next
	// sync does.
	for _, e := range []*Engine{a, b} {
		reconfigure(t, e, func(o *Options) { o.Fsync = FsyncAlways })
	}
	var bDirSyncs atomic.Int64
	dirErr := errors.New("b's directory will not sync")
	b.rewriteSyncDir = func(string) error { bDirSyncs.Add(1); return dirErr }
	require.NoError(t, b.StartRewrite())
	drive(b)
	require.Equal(t, filepath.Dir(b.aof.path), b.unsyncedLogDir)
	require.Equal(t, int64(1), bDirSyncs.Load())
	require.Equal(t, "OK", on(t, a, "SET", "synced", "a"))
	require.NoError(t, a.FlushAOF())
	aPath := a.aof.path
	require.NoError(t, a.CloseAOF())
	require.NoError(t, a.OpenAOF(aPath))
	assert.Equal(t, filepath.Dir(b.aof.path), b.unsyncedLogDir, "a leaves b's pending directory sync")
	assert.Equal(t, int64(1), bDirSyncs.Load())
	b.rewriteSyncDir = syncDir
	require.Equal(t, "OK", on(t, b, "SET", "synced", "b"))
	require.NoError(t, b.FlushAOF())
	assert.Empty(t, b.unsyncedLogDir, "b's own sync retries it")
	for _, e := range []*Engine{a, b} {
		reconfigure(t, e, func(o *Options) { o.Fsync = "" })
	}

	// Side by side, each on a goroutine of its own and written to while it
	// rewrites: a's rewrites fail on a's disk three times running, which earns
	// a's automatic rewrites Redis's wait, while b's succeed. Each engine's
	// INFO reports its own outcome and I/O counters, and each log still
	// replays to its own engine.
	a.rewriteFileWrite = func(*os.File, []byte) (int, error) { return 0, diskErr }
	bRewrites := b.aof.rewrites
	aWriteErrors, bWriteErrors := field(a, "aof_rewrite_write_errors"), field(b, "aof_rewrite_write_errors")
	aErrors, err := strconv.Atoi(aWriteErrors)
	require.NoError(t, err)
	var wg sync.WaitGroup
	for _, side := range []*Engine{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := range 3 {
				if !assert.NoError(t, side.StartRewrite()) {
					return
				}
				for i := 0; side.RewriteActive(); i++ {
					var w replyWriter
					cmd := &Command{Cmd: "SET", Args: []string{"side:" + strconv.Itoa(i%20), strconv.Itoa(r)}}
					if !assert.NoError(t, side.evalAndResponse(cmd, &w)) || !assert.NoError(t, side.FlushAOF()) ||
						!assert.Less(t, i, 100000, "the rewrite did not end") {
						return
					}
					time.Sleep(20 * time.Microsecond)
				}
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, "err", field(a, "aof_last_bgrewrite_status"))
	assert.Equal(t, "3", field(a, "aof_rewrites_consecutive_failures"))
	assert.Equal(t, "ok", field(b, "aof_last_bgrewrite_status"))
	assert.Equal(t, "0", field(b, "aof_rewrites_consecutive_failures"))
	assert.Equal(t, bRewrites+3, b.aof.rewrites)
	assert.Equal(t, strconv.Itoa(aErrors+3), field(a, "aof_rewrite_write_errors"),
		"each of a's three rewrites failed on its first write, counted on a")
	assert.Equal(t, bWriteErrors, field(b, "aof_rewrite_write_errors"), "and not on b")
	now := time.Now()
	assert.True(t, a.rewriteLimited(now), "three failures in a row hold a's automatic rewrites back")
	assert.False(t, b.rewriteLimited(now), "and not b's")
	a.rewriteFileWrite = writeLog
	replays(a)
	replays(b)
	assert.False(t, bystander.RewriteActive())
	assert.Nil(t, bystander.aof.file)
	assert.Equal(t, bystanderOutcome, bystander.rewriteOutcome, "neither's failures are the bystander's")
	assert.Equal(t, bystanderRewrites, bystander.aof.rewrites, "nor are their rewrites")
}

// midTransaction is a transport whose own command, run in its place inside an
// EXEC, calls answer.
type midTransaction struct {
	t      *testing.T
	answer func()
}

func (m *midTransaction) RESP3() bool { return false }
func (m *midTransaction) AnswerConnection(_ *Command, w io.ReadWriter) {
	m.answer()
	n, err := w.Write(constant.RespOk)
	assert.NoError(m.t, err)
	assert.Equal(m.t, len(constant.RespOk), n, "a short write")
}
