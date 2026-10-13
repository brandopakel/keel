package core

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file sorts after every file of the package's code, so that its tests,
// compiled into package core, move none of the handlers in the test binary
// the paired command-path job measures (docs/embedding-plan.md, phase 4).

// TestEveryRecordNameReplicates: every record name the log takes, as the
// command corpus and the persistence and replication goldens hold them, is
// one a replica applies. The replication stream of protocol 2 is the log's
// records, so a write that stages a record a replica refuses stops every
// replica at the first one; a family that adds a record name fails here until
// replicationOperation has it.
func TestEveryRecordNameReplicates(t *testing.T) {
	t.Parallel()
	seen := map[string]string{}
	note := func(name, where string) {
		if _, ok := seen[name]; !ok {
			seen[name] = where
		}
	}
	_, corpus := loadCommandGoldens(t)
	for name, c := range corpus {
		for _, step := range c.Steps {
			for _, record := range step.Records {
				note(record[0], "command corpus case "+name)
			}
		}
	}

	logs, err := filepath.Glob(filepath.Join(persistenceGoldenDir, "*.aof.gz"))
	require.NoError(t, err)
	require.NotEmpty(t, logs)
	for _, path := range logs {
		body := gunzipFile(t, path)
		for len(body) > 0 {
			cmd, n, err := ParseCmd(body)
			require.NoError(t, err, path)
			require.Positive(t, n, path)
			note(cmd.Cmd, path)
			body = body[n:]
		}
	}

	transcripts, err := filepath.Glob(filepath.Join(replicationGoldenDir, "*.txt.gz"))
	require.NoError(t, err)
	require.NotEmpty(t, transcripts)
	for _, path := range transcripts {
		lines := bufio.NewScanner(bytes.NewReader(gunzipFile(t, path)))
		lines.Buffer(nil, 1<<26)
		for lines.Scan() {
			line := lines.Text()
			if !strings.HasPrefix(line, `  "`) {
				continue
			}
			quoted := line[2:]
			end := strings.Index(quoted[1:], `"`) + 2
			name, err := strconv.Unquote(quoted[:end])
			require.NoError(t, err, "%s: %q", path, line)
			note(name, path)
		}
		require.NoError(t, lines.Err())
	}

	require.Contains(t, seen, "SET", "the fixtures were read")
	for name, where := range seen {
		if name == "MULTI" || name == "EXEC" || isOpaqueReplicationCommand(name) {
			continue
		}
		assert.True(t, replicationOperation(name), "%s is recorded (%s) but a replica refuses it: add it to replicationOperation", name, where)
	}
}

func gunzipFile(t *testing.T, path string) []byte {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	z, err := gzip.NewReader(f)
	require.NoError(t, err)
	body, err := io.ReadAll(z)
	require.NoError(t, err)
	return body
}

// TestReplicaAppliesASpopThatPopsNothing: a SPOP that pops nothing - a
// missing key, with and without a count, or a count of 0 - records nothing,
// so a protocol 2 replica, whose stream is the log, is not handed a SPOP it
// refuses; a SPOP that pops records the SREM of what it popped, which the
// replica applies.
func TestReplicaAppliesASpopThatPopsNothing(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	setupReplicationV2On(t, e)
	runOn(t, e, "SADD", "s", "a", "b", "c")
	frames := snapshotV2On(t, e)
	base, epoch := frames[0].To, frames[0].Epoch
	for _, cmd := range [][]string{
		{"SPOP", "missing"}, {"SPOP", "missing", "2"}, {"SPOP", "missing", "0"}, {"SPOP", "s", "0"},
		{"SPOP", "s"}, {"SPOP", "s", "1"},
	} {
		reply := rawReplyOn(t, e, cmd[0], cmd[1:]...)
		require.NotEqual(t, byte('-'), reply[0], "%v: %q", cmd, reply)
	}
	delta := pullV2On(t, e, epoch, base, "", 0)
	require.NotContains(t, string(delta.Body), "SPOP", "a SPOP is recorded as the SREM of what it popped, or not at all")
	want := runOn(t, e, "SMEMBERS", "s")
	require.Len(t, want, 1)
	becomeReplicaV2On(t, e)
	for _, frame := range frames {
		require.NoError(t, e.ApplyReplication(frame))
	}
	require.NoError(t, e.ApplyReplication(delta))
	require.True(t, e.replicaReady)
	require.Equal(t, want, runOn(t, e, "SMEMBERS", "s"))
}

// stringStateOn is each of keys' value and expiry instant on e, read
// without reaping, for comparing a replica with its primary.
func stringStateOn(e *Engine, keys ...string) map[string]string {
	state := map[string]string{}
	for _, key := range keys {
		obj := e.dictStore.Peek(key)
		if obj == nil {
			state[key] = "(none)"
			continue
		}
		at, expires := e.dictStore.GetExpiry(key)
		state[key] = obj.Value + " expires " + strconv.FormatBool(expires) + " " + strconv.FormatUint(at, 10)
	}
	return state
}

// TestReplicaAppliesTheStringCommandsRecords: a protocol 2 replica applies
// the records of every string command, each as the primary staged it -
// APPEND and SETRANGE as themselves, GETSET as SET, GETDEL as DEL, GETEX's
// expiry as PEXPIREAT, PERSIST or DEL, INCRBYFLOAT as SET with KEEPTTL,
// MSETNX as MSET - and ends with the primary's strings and expiries.
func TestReplicaAppliesTheStringCommandsRecords(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	setupReplicationV2On(t, e)
	frames := snapshotV2On(t, e)
	base, epoch := frames[0].To, frames[0].Epoch
	for _, cmd := range [][]string{
		{"SET", "a", "hello"}, {"APPEND", "a", " world"}, {"APPEND", "fresh", "x"},
		{"SETRANGE", "a", "0", "J"}, {"SETRANGE", "padded", "3", "x"},
		{"GETSET", "a", "replaced"}, {"SET", "gone", "v"}, {"GETDEL", "gone"},
		{"SET", "g", "v"}, {"GETEX", "g", "EX", "100000"}, {"SET", "p", "v", "EX", "100000"},
		{"GETEX", "p", "PERSIST"}, {"SET", "past", "v"}, {"GETEX", "past", "PXAT", "1"},
		{"INCRBYFLOAT", "f", "10.5"}, {"INCRBYFLOAT", "f", "0.1"}, {"SET", "t", "5", "EX", "100000"},
		{"INCRBYFLOAT", "t", "0.5"}, {"MSETNX", "m1", "a", "m2", "b"}, {"MSETNX", "m1", "x", "m3", "y"},
	} {
		reply := rawReplyOn(t, e, cmd[0], cmd[1:]...)
		require.NotEqual(t, byte('-'), reply[0], "%v: %q", cmd, reply)
	}
	delta := pullV2On(t, e, epoch, base, "", 0)
	keys := []string{"a", "fresh", "padded", "gone", "g", "p", "past", "f", "t", "m1", "m2", "m3"}
	want := stringStateOn(e, keys...)
	require.Equal(t, "replaced expires false 0", want["a"])
	require.Equal(t, "\x00\x00\x00x expires false 0", want["padded"])
	require.Equal(t, "10.6 expires false 0", want["f"])
	require.Equal(t, "(none)", want["past"])
	require.Contains(t, want["t"], "5.5 expires true")
	becomeReplicaV2On(t, e)
	for _, frame := range frames {
		require.NoError(t, e.ApplyReplication(frame))
	}
	require.NoError(t, e.ApplyReplication(delta))
	require.True(t, e.replicaReady)
	require.Equal(t, want, stringStateOn(e, keys...))
}

// TestReplicaAppliesTheRedis8StringCommandsRecords: a protocol 2 replica
// applies the records of the string commands Redis 8 added - SET's
// conditions as SET, DELEX as DEL, MSETEX as a DEL of a replaced key and SET
// and PEXPIREAT per key, INCREX as SET with KEEPTTL, SET then PEXPIREAT, a
// plain SET under PERSIST, or the DEL an expiry already past amounts to -
// and ends with the primary's strings and expiries.
func TestReplicaAppliesTheRedis8StringCommandsRecords(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	setupReplicationV2On(t, e)
	frames := snapshotV2On(t, e)
	base, epoch := frames[0].To, frames[0].Epoch
	for _, cmd := range [][]string{
		{"SET", "k", "v"}, {"SET", "k", "w", "IFEQ", "v"}, {"SET", "k", "x", "IFNE", "nomatch", "EX", "100000"},
		{"SET", "k", "y", "IFDEQ", stringDigest("x"), "KEEPTTL"}, {"SET", "d", "v"}, {"DELEX", "d", "IFEQ", "v"},
		{"SET", "d2", "v"}, {"DELEX", "d2"}, {"HSET", "h", "f", "v"},
		{"MSETEX", "3", "h", "1", "m", "2", "k", "3", "PX", "100000000"}, {"MSETEX", "1", "m", "4", "KEEPTTL"},
		{"INCREX", "n", "BYINT", "5"}, {"INCREX", "n", "EX", "100000"}, {"INCREX", "n", "ENX", "EX", "5"},
		{"INCREX", "n", "PERSIST"}, {"INCREX", "fl", "BYFLOAT", "10.5"}, {"INCREX", "fl", "BYFLOAT", "0.1", "UBOUND", "11"},
		{"INCREX", "sat", "BYINT", "50", "UBOUND", "10", "SATURATE"}, {"SET", "gone", "1"}, {"INCREX", "gone", "EXAT", "1"},
	} {
		reply := rawReplyOn(t, e, cmd[0], cmd[1:]...)
		require.NotEqual(t, byte('-'), reply[0], "%v: %q", cmd, reply)
	}
	delta := pullV2On(t, e, epoch, base, "", 0)
	keys := []string{"k", "d", "d2", "h", "m", "n", "fl", "sat", "gone"}
	want := stringStateOn(e, keys...)
	require.Contains(t, want["k"], "3 expires true")
	require.Contains(t, want["m"], "4 expires true")
	require.Equal(t, "8 expires false 0", want["n"])
	require.Equal(t, "10.6 expires false 0", want["fl"])
	require.Equal(t, "10 expires false 0", want["sat"])
	require.Equal(t, "(none)", want["gone"])
	becomeReplicaV2On(t, e)
	for _, frame := range frames {
		require.NoError(t, e.ApplyReplication(frame))
	}
	require.NoError(t, e.ApplyReplication(delta))
	require.True(t, e.replicaReady)
	require.Equal(t, want, stringStateOn(e, keys...))
	_, hashLeft := e.space.OwnerOf("h")
	require.True(t, hashLeft, "h holds the string MSETEX stored")
	require.Equal(t, "string", runOn(t, e, "TYPE", "h"))
}
