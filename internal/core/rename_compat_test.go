package core

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The server was called memkv before it was called keel, and two things
// crossed that boundary: a command name written into every append-only file,
// and the default name of the file itself. Both fail silently if they are
// dropped - a log that will not replay, or a log nobody looks at - so both are
// pinned here rather than left to be noticed by whoever restarts first.

// TestLegacyDumpCommandNamesStillReplay covers logs written before the rename,
// every one of which records MEMKV.RESTORE.
func TestLegacyDumpCommandNamesStillReplay(t *testing.T) {
	ResetStores()
	run(t, "PFADD", "h", "a", "b", "c")
	before := run(t, "PFCOUNT", "h")

	payload, ok := run(t, "MEMKV.DUMP", "h").(string)
	assert.True(t, ok, "the old DUMP name still answers")

	ResetStores()
	assert.Equal(t, "OK", run(t, "MEMKV.RESTORE", "h", payload),
		"the old RESTORE name still loads, or no log written before the rename replays")
	assert.Equal(t, before, run(t, "PFCOUNT", "h"))
}

func TestNewAndOldDumpNamesAreTheSameCommand(t *testing.T) {
	ResetStores()
	run(t, "PFADD", "h", "x", "y")

	viaNew, _ := run(t, "KEEL.DUMP", "h").(string)
	viaOld, _ := run(t, "MEMKV.DUMP", "h").(string)
	assert.Equal(t, viaOld, viaNew, "one command, two names")

	ResetStores()
	assert.Equal(t, "OK", run(t, "KEEL.RESTORE", "h", viaOld),
		"a payload dumped under either name loads under either name")
}

// TestRewriteWritesTheNewNameOnly: the old name is read, never written, so a
// rewritten log stops mentioning memkv at all.
func TestRewriteWritesTheNewNameOnly(t *testing.T) {
	path := withAOF(t, func() {
		run(t, "PFADD", "h", "a", "b")
		assert.NoError(t, RewriteAOF())
	})

	body, err := os.ReadFile(path)
	assert.NoError(t, err)
	assert.Contains(t, string(body), "KEEL.RESTORE")
	assert.NotContains(t, string(body), "MEMKV.RESTORE",
		"a rewrite produces the shortest log for the current state, in current names")
}

// TestALogWrittenUnderTheOldNameStillReplays is the whole-file version: a log
// full of legacy command names restores the keyspace it recorded.
func TestALogWrittenUnderTheOldNameStillReplays(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memkv-master.aof")

	ResetStores()
	run(t, "PFADD", "h", "a", "b", "c")
	payload, _ := run(t, "KEEL.DUMP", "h").(string)
	expected := run(t, "PFCOUNT", "h")

	// Hand-built in the shape a pre-rename server wrote.
	legacy := appendCommand(nil, "SET", "plain", "value")
	legacy = appendCommand(legacy, "MEMKV.RESTORE", "h", payload)
	assert.NoError(t, os.WriteFile(path, legacy, 0o644))

	ResetStores()
	applied, err := LoadAOF(path)
	assert.NoError(t, err)
	assert.Equal(t, 2, applied)
	assert.Equal(t, "value", run(t, "GET", "plain"))
	assert.Equal(t, expected, run(t, "PFCOUNT", "h"),
		"a HyperLogLog restored from a legacy log estimates what it did before")
}
