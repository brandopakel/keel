package server

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/brandopakel/keel/internal/config"
	"github.com/brandopakel/keel/internal/core"
	"github.com/brandopakel/keel/internal/data_structure"
)

// The default log was ./memkv-master.aof before the rename and is
// ./keel-master.aof now. A restart that looked only at the new name would
// replay nothing and serve an empty keyspace beside the old log, silently.
func TestAOFReadPath(t *testing.T) {
	write := func(t *testing.T, path string) {
		t.Helper()
		assert.NoError(t, os.WriteFile(path, []byte("*1\r\n$4\r\nPING\r\n"), 0o644))
	}

	t.Run("reads the current name when it is there", func(t *testing.T) {
		dir := t.TempDir()
		current := filepath.Join(dir, "keel-master.aof")
		legacy := filepath.Join(dir, "memkv-master.aof")
		write(t, current)
		assert.Equal(t, current, aofReadPath(current, legacy))
	})

	t.Run("falls back to the name used before the rename", func(t *testing.T) {
		dir := t.TempDir()
		current := filepath.Join(dir, "keel-master.aof")
		legacy := filepath.Join(dir, "memkv-master.aof")
		write(t, legacy)
		assert.Equal(t, legacy, aofReadPath(current, legacy),
			"a log written before the rename must still be found")
	})

	t.Run("prefers the current name when both exist", func(t *testing.T) {
		dir := t.TempDir()
		current := filepath.Join(dir, "keel-master.aof")
		legacy := filepath.Join(dir, "memkv-master.aof")
		write(t, current)
		write(t, legacy)
		assert.Equal(t, current, aofReadPath(current, legacy),
			"the old name is a fallback, not a merge")
	})

	t.Run("reports the current name when neither exists", func(t *testing.T) {
		dir := t.TempDir()
		current := filepath.Join(dir, "keel-master.aof")
		legacy := filepath.Join(dir, "memkv-master.aof")
		assert.Equal(t, current, aofReadPath(current, legacy),
			"a first start writes to the current name")
	})
}

// TestMigratingFromTheLegacyLogSurvivesASecondRestart.
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

	oldEnabled, oldName := config.AOFEnabled, config.AOFFileName
	oldLegacy := config.LegacyAOFFileName
	config.AOFEnabled, config.AOFFileName, config.LegacyAOFFileName = true, current, legacy
	defer func() {
		config.AOFEnabled, config.AOFFileName = oldEnabled, oldName
		config.LegacyAOFFileName = oldLegacy
	}()

	// First start: reads the legacy log, then writes what it read into the
	// current one before anything else appends to it.
	core.ResetStores()
	assert.NoError(t, StartAOF())
	assert.Equal(t, 1, data_structure.TotalKeys(), "the legacy key is here after one restart")
	assert.NoError(t, core.EvalAndResponse(
		&core.Command{Cmd: "SET", Args: []string{"new-k", "added"}}, &bytes.Buffer{}))
	assert.NoError(t, core.FlushAOF())
	assert.NoError(t, core.CloseAOF())

	// Second start: the current file now exists and takes precedence.
	core.ResetStores()
	assert.NoError(t, StartAOF())
	defer core.CloseAOF()
	assert.Equal(t, 2, data_structure.TotalKeys(),
		"the key that lived only in the legacy log has to survive the file swap")
}
