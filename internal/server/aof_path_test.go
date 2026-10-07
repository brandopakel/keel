package server

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/brandopakel/keel/internal/core"
)

// TestMigratingFromTheLegacyLogSurvivesASecondRestart: the server's legacy log,
// migrated by the log's startup it passes the legacy name to (StartAOF).
//
// The fallback on its own does not save the data, it delays losing it. Start
// one reads memkv-master.aof and opens an empty keel-master.aof; start two sees
// keel-master.aof present, prefers it, and replays only what was written after
// the migration. Everything that lived solely in the old log is gone, with the
// old log still sitting there looking like a backup.
//
// Reproduced before it was fixed: `legacy-k` was readable after the first
// restart and absent after the second.
func TestMigratingFromTheLegacyLogSurvivesASecondRestart(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "memkv-master.aof")
	current := filepath.Join(dir, "keel-master.aof")

	assert.NoError(t, os.WriteFile(legacy,
		[]byte("*3\r\n$3\r\nSET\r\n$8\r\nlegacy-k\r\n$5\r\nvalue\r\n"), 0o644))

	options := core.Options{AppendOnly: true, AppendFilename: current}
	keys := func(e *core.Engine) string {
		var reply bytes.Buffer
		assert.NoError(t, e.EvalAndResponse(&core.Command{Cmd: "DBSIZE"}, &reply))
		return reply.String()
	}

	// First start: reads the legacy log, then writes what it read into the
	// current one before anything else appends to it.
	e := newTestEngine(t, options)
	assert.NoError(t, e.StartAOF(context.Background(), legacy))
	assert.Equal(t, ":1\r\n", keys(e), "the legacy key is here after one restart")
	assert.NoError(t, e.EvalAndResponse(
		&core.Command{Cmd: "SET", Args: []string{"new-k", "added"}}, &bytes.Buffer{}))
	assert.NoError(t, e.FlushAOF())
	assert.NoError(t, e.Close(), "a restart: the log closed, and its lock released")

	// Second start, on an engine of its own as a restarted server's is: the
	// current file now exists and takes precedence.
	e = newTestEngine(t, options)
	assert.NoError(t, e.StartAOF(context.Background(), legacy))
	assert.Equal(t, ":2\r\n", keys(e),
		"the key that lived only in the legacy log has to survive the file swap")
}
