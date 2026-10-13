package core

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// This file sorts after every file of the package's code, so that its tests,
// compiled into package core, move none of the handlers in the test binary
// the paired command-path job measures (docs/phase4-family-recipe.md,
// step 5).

// replicaRefusedWrites are the writes the typed operations of phase 4 serve,
// each as a client sends it, on the keys replicaSeed gives the replica: each
// is refused by a replica before its operation runs. A family that converts
// writes adds them here, with the forms a fast path could take.
var replicaRefusedWrites = [][]string{
	// Strings (step 4.3b).
	{"SET", "k", "v"}, {"SET", "missing", "v"}, {"SET", "k", "v", "EX", "100"}, {"SET", "k", "v", "PX", "100000"},
	{"SET", "k", "v", "NX"}, {"SET", "k", "v", "XX"}, {"SET", "k", "v", "GET"}, {"SET", "k", "v", "KEEPTTL"},
	{"SETEX", "k", "100", "v"}, {"PSETEX", "k", "100000", "v"}, {"SETNX", "missing", "v"},
	{"MSET", "k", "1", "missing", "2"}, {"INCR", "n"}, {"INCR", "missing"}, {"DECR", "n"},
	{"INCRBY", "n", "5"}, {"DECRBY", "n", "5"},
	// The string commands (step 4.3c).
	{"APPEND", "k", "x"}, {"SETRANGE", "k", "0", "x"}, {"GETDEL", "k"}, {"GETEX", "k"},
	{"GETEX", "k", "EX", "100"}, {"GETEX", "k", "PERSIST"}, {"GETSET", "k", "v"},
	{"MSETNX", "missing", "1", "other", "2"}, {"INCRBYFLOAT", "n", "1.5"},
}

// replicaSeed is the state the replica has from its primary: a key of each
// type the refused writes name.
var replicaSeed = [][]string{
	{"SET", "k", "v"}, {"SET", "n", "10"}, {"HSET", "h", "f", "v", "c", "1"}, {"SADD", "s", "a", "b"},
	{"ZADD", "z", "1", "a", "2", "b"}, {"RPUSH", "l", "a", "b"},
}

const readOnlyReply = "-READONLY You can't write against a read only replica.\r\n"

// replicaState is what a client of e can read of the seeded keys.
func replicaState(t *testing.T, e *Engine) []interface{} {
	t.Helper()
	return []interface{}{
		runOn(t, e, "DBSIZE"), runOn(t, e, "GET", "k"), runOn(t, e, "GET", "n"), runOn(t, e, "TTL", "k"),
		runOn(t, e, "HLEN", "h"), runOn(t, e, "HGET", "h", "f"), runOn(t, e, "HGET", "h", "c"),
		runOn(t, e, "SCARD", "s"), runOn(t, e, "SMISMEMBER", "s", "a", "b"), runOn(t, e, "ZRANGE", "z", "0", "-1", "WITHSCORES"),
		runOn(t, e, "LRANGE", "l", "0", "-1"),
	}
}

// genericArguments are arguments of a count name accepts, so that every write
// of the table reaches the replica's refusal, which comes before anything
// reads them.
func genericArguments(t *testing.T, name string) []string {
	t.Helper()
	entry := commands[name]
	for n := 0; n <= 16; n++ {
		if entry.counted(n) {
			args := make([]string, n)
			for i := range args {
				args[i] = "1"
			}
			if n > 0 {
				args[0] = "k"
			}
			return args
		}
	}
	t.Fatalf("%s accepts no count up to 16", name)
	return nil
}

// TestReplicaRefusesEveryWrite: a replica answers every write - each the
// typed operations serve, in the forms a client sends them, and every
// command of the write table - with READONLY, alone and queued in MULTI,
// before and after it has its primary's state, and changes nothing. The
// refusal comes ahead of every handler and operation (EvalAndResponse,
// replicaCommandError), so no family's conversion can answer a write first;
// cmd/keel's TestReplicaRefusesWritesOnEveryPath holds the server's
// execution paths, ordered concurrent appends included, to the same.
func TestReplicaRefusesEveryWrite(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	oldReady, oldUpdated := e.replicaReady, e.replicaUpdated
	t.Cleanup(func() { e.replicaReady, e.replicaUpdated = oldReady, oldUpdated })
	reconfigure(t, e, func(o *Options) { o.ReplicaOf = "primary.test:6379" })
	e.replicaApplying = true
	for _, cmd := range replicaSeed {
		runOn(t, e, cmd[0], cmd[1:]...)
	}
	e.replicaApplying = false
	e.replicaReady, e.replicaUpdated = true, time.Now()
	before := replicaState(t, e)

	writes := append([][]string(nil), replicaRefusedWrites...)
	for _, cmd := range writes {
		require.True(t, writeCommands[cmd[0]], "%s is a write the log takes", cmd[0])
	}
	names := make([]string, 0, len(writeCommands))
	for name := range writeCommands {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		writes = append(writes, append([]string{name}, genericArguments(t, name)...))
	}

	for _, ready := range []bool{false, true} {
		e.replicaReady, e.replicaUpdated = ready, time.Now()
		for _, cmd := range writes {
			require.Equal(t, readOnlyReply, string(rawReplyOn(t, e, cmd[0], cmd[1:]...)), "ready %v: %q", ready, cmd)
			s := &session{t: t, e: e}
			require.Equal(t, "+OK\r\n", s.send("MULTI"))
			require.Equal(t, readOnlyReply, s.send(cmd...), "ready %v, queued: %q", ready, cmd)
			require.Equal(t, "-EXECABORT Transaction discarded because of previous errors.\r\n", s.send("EXEC"),
				"ready %v: %q", ready, cmd)
		}
	}
	e.replicaReady, e.replicaUpdated = true, time.Now()
	require.Equal(t, before, replicaState(t, e), "a refused write changed the replica")
	require.NotContains(t, strings.Join(names, " "), "KEEL.PROMOTE", "a replica must still take its promotion")
}
