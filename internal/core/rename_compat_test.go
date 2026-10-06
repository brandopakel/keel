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
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "PFADD", "h", "a", "b", "c")
	before := runOn(t, e, "PFCOUNT", "h")

	payload, ok := runOn(t, e, "MEMKV.DUMP", "h").(string)
	assert.True(t, ok, "the old DUMP name still answers")

	e.resetStores()
	assert.Equal(t, "OK", runOn(t, e, "MEMKV.RESTORE", "h", payload),
		"the old RESTORE name still loads, or no log written before the rename replays")
	assert.Equal(t, before, runOn(t, e, "PFCOUNT", "h"))
}

func TestNewAndOldDumpNamesAreTheSameCommand(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "PFADD", "h", "x", "y")

	before := runOn(t, e, "PFCOUNT", "h")

	viaNew, _ := runOn(t, e, "KEEL.DUMP", "h").(string)
	viaOld, _ := runOn(t, e, "MEMKV.DUMP", "h").(string)
	assert.Equal(t, viaOld, viaNew, "one command, two names")

	e.resetStores()
	assert.Equal(t, "OK", runOn(t, e, "KEEL.RESTORE", "h", viaOld),
		"a payload dumped under either name loads under either name")
	// OK on its own would pass for a restore that returned success and stored
	// nothing, which is the failure worth catching here.
	assert.Equal(t, before, runOn(t, e, "PFCOUNT", "h"),
		"and the restored structure estimates what it did before")
	assert.Equal(t, "hll", runOn(t, e, "TYPE", "h"))
}

// TestRewriteWritesTheNewNameOnly: the old name is read, never written, so a
// rewritten log stops mentioning memkv at all.
func TestRewriteWritesTheNewNameOnly(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := withAOFOn(t, e, func() {
		runOn(t, e, "PFADD", "h", "a", "b")
		assert.NoError(t, e.RewriteAOF())
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
	t.Parallel()
	e := newTestEngine(t, Options{})
	dir := t.TempDir()
	path := filepath.Join(dir, "memkv-master.aof")

	runOn(t, e, "PFADD", "h", "a", "b", "c")
	payload, _ := runOn(t, e, "KEEL.DUMP", "h").(string)
	expected := runOn(t, e, "PFCOUNT", "h")

	// Hand-built in the shape a pre-rename server wrote.
	legacy := appendCommand(nil, "SET", "plain", "value")
	legacy = appendCommand(legacy, "MEMKV.RESTORE", "h", payload)
	assert.NoError(t, os.WriteFile(path, legacy, 0o644))

	e.resetStores()
	applied, err := e.LoadAOF(path)
	assert.NoError(t, err)
	assert.Equal(t, 2, applied)
	assert.Equal(t, "value", runOn(t, e, "GET", "plain"))
	assert.Equal(t, expected, runOn(t, e, "PFCOUNT", "h"),
		"a HyperLogLog restored from a legacy log estimates what it did before")
}

// TestALiveLegacyRestoreIsRecordedUnderTheNewName.
//
// The alias is for reading, not for writing. A command is appended to the log
// as it arrived, so a client sending MEMKV.RESTORE would put the old name into
// a file created after the rename - and every rewrite of that file would carry
// it forward, so the alias could never be retired.
func TestALiveLegacyRestoreIsRecordedUnderTheNewName(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "PFADD", "h", "a", "b", "c")
	payload, _ := runOn(t, e, "KEEL.DUMP", "h").(string)
	expected := runOn(t, e, "PFCOUNT", "h")

	path := withAOFOn(t, e, func() {
		assert.Equal(t, "OK", runOn(t, e, "MEMKV.RESTORE", "h", payload))
	})

	body, err := os.ReadFile(path)
	assert.NoError(t, err)
	assert.Contains(t, string(body), "KEEL.RESTORE",
		"the log records the current name")
	assert.NotContains(t, string(body), "MEMKV.RESTORE",
		"a log written after the rename must not carry the old name forward")

	restartOn(t, e, path)
	assert.Equal(t, expected, runOn(t, e, "PFCOUNT", "h"), "and it still replays")
}
