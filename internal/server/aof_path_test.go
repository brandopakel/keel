package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
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
