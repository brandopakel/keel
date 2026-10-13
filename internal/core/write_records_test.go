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
