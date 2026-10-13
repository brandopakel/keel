package main

import (
	"path/filepath"
	"testing"
	"time"
)

// replicaWrites are writes a replica refuses, each as a client sends it, on
// the keys replicaWritesSeed gives the primary: the writes phase 4's typed
// operations serve, in the forms a fast path could take. A family that
// converts writes adds them here and to internal/core's
// replicaRefusedWrites.
var replicaWrites = [][]string{
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
	// Redis 8's string commands (step 4.3d).
	{"DELEX", "k"}, {"DELEX", "k", "IFEQ", "v"}, {"MSETEX", "1", "k", "v", "EX", "100"}, {"INCREX", "n"},
	{"INCREX", "missing", "BYFLOAT", "1.5"}, {"SET", "k", "v", "IFEQ", "v"}, {"SET", "k", "v", "IFNE", "x"},
	{"SET", "k", "v", "IFDEQ", "0000000000000000"}, {"SET", "k", "v", "IFDNE", "0000000000000000"},
	// Keys and expiry (step 4.4).
	{"DEL", "k"}, {"DEL", "missing"}, {"UNLINK", "k"}, {"EXPIRE", "k", "100"}, {"EXPIRE", "k", "100", "NX"},
	{"PEXPIRE", "k", "100000"}, {"EXPIREAT", "k", "9999999999"}, {"PEXPIREAT", "k", "9999999999000"},
	{"EXPIRE", "k", "-1"}, {"PERSIST", "k"}, {"FLUSHDB"}, {"FLUSHDB", "ASYNC"},
	// The key commands (step 4.4).
	{"RENAME", "k", "renamed"}, {"RENAMENX", "k", "renamed"}, {"COPY", "k", "copied"},
	{"COPY", "k", "n", "REPLACE"},
	// Hashes (step 4.5).
	{"HSET", "h", "f", "x"}, {"HSET", "h", "g", "y"}, {"HSET", "missing", "f", "x"}, {"HSETNX", "h", "g", "y"},
	{"HDEL", "h", "f"}, {"HINCRBY", "h", "c", "1"},
	// The hash commands (step 4.5).
	{"HMSET", "h", "f", "x"}, {"HINCRBYFLOAT", "h", "c", "1.5"}, {"HGETDEL", "h", "FIELDS", "1", "f"},
	// Sets (step 4.6).
	{"SADD", "s", "c"}, {"SADD", "missing", "c"}, {"SREM", "s", "a"}, {"SPOP", "s"}, {"SPOP", "s", "2"},
}

var replicaWritesSeed = [][]string{
	{"SET", "k", "v"}, {"SET", "n", "10"}, {"HSET", "h", "f", "v"}, {"SADD", "s", "a"},
	{"ZADD", "z", "1", "a"}, {"RPUSH", "l", "a"},
}

// replicaWritesState is what a client reads of the seeded keys.
var replicaWritesState = [][]string{
	{"DBSIZE"}, {"GET", "k"}, {"GET", "n"}, {"TTL", "k"}, {"HGET", "h", "f"}, {"SCARD", "s"},
	{"ZSCORE", "z", "a"}, {"LLEN", "l"}, {"EXISTS", "missing"},
}

const readOnly = "-READONLY You can't write against a read only replica."

// TestReplicaRefusesWritesOnEveryPath: a replica server refuses every write
// with READONLY, before and after it has its primary's state, however the
// write reaches it - alone, pipelined with the others, queued in MULTI - with
// each replication protocol, and with ordered concurrent appends, the path
// that runs admitted string commands while earlier appends are in flight. It
// changes nothing: the replica reads as its primary does.
//
// soak.py's check of the same refusal once failed because its replica never
// listened: a harness port collision, which scripts/validation_lib.py now
// refuses (#183's "Recovery on xfs", run 38341148820).
func TestReplicaRefusesWritesOnEveryPath(t *testing.T) {
	t.Parallel()
	for _, mode := range []struct {
		name, protocol string
		args           []string
	}{
		{"protocol-1", "1", nil},
		{"protocol-2-concurrent-append", "2", []string{"-aof-async-append", "-aof-concurrent-append", "-appendfsync", "always"}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()
			primary := startTestServer(t, append([]string{"-appendonly", "-replication-protocol", mode.protocol,
				"-replication-feed", "-requirepass-env", "KEEL_TEST_PASSWORD",
				"-appendfilename", filepath.Join(t.TempDir(), "primary")}, mode.args...)...)
			pc, pr := connectTest(t, primary)
			call(t, pc, pr, "AUTH", "integration-secret")
			for _, cmd := range append(replicaWritesSeed, []string{"SET", "synced", "yes"}) {
				if got := call(t, pc, pr, cmd...); len(got) > 0 && got[0] == '-' {
					t.Fatalf("%q on the primary: %s", cmd, got)
				}
			}
			replica := startTestServer(t, append([]string{"-appendonly", "-replication-protocol", mode.protocol,
				"-requirepass-env", "KEEL_TEST_PASSWORD", "-primary-password-env", "KEEL_TEST_PASSWORD",
				"-replicaof", primary.addr, "-appendfilename", filepath.Join(t.TempDir(), "replica")}, mode.args...)...)
			rc, rr := connectTest(t, replica)
			call(t, rc, rr, "AUTH", "integration-secret")

			refuses := func(when string) {
				t.Helper()
				for _, cmd := range replicaWrites {
					if got := call(t, rc, rr, cmd...); got != readOnly {
						t.Fatalf("%s, %q: %s", when, cmd, got)
					}
				}
				for i, got := range pipeline(t, rc, rr, replicaWrites...) {
					if got != readOnly {
						t.Fatalf("%s, pipelined %q: %s", when, replicaWrites[i], got)
					}
				}
				for _, cmd := range replicaWrites {
					expectReplies(t, pipeline(t, rc, rr, []string{"MULTI"}, cmd, []string{"EXEC"}), "+OK", readOnly, execAbort)
				}
			}
			refuses("before its primary's state")
			deadline := time.Now().Add(10 * time.Second)
			for call(t, rc, rr, "GET", "synced") != "yes" {
				if time.Now().After(deadline) {
					t.Fatal("replica did not synchronise")
				}
				time.Sleep(25 * time.Millisecond)
			}
			refuses("with its primary's state")
			for _, cmd := range replicaWritesState {
				if want, got := call(t, pc, pr, cmd...), call(t, rc, rr, cmd...); got != want {
					t.Fatalf("%q: the replica reads %s, its primary %s", cmd, got, want)
				}
			}
		})
	}
}
