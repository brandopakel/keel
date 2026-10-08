package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheServerTakesNoLockBesideItsLog: an engine core.Open makes takes a
// lock beside its log, but the server starts its log without Open and takes
// none, as Redis takes none (docs/embedding-plan.md, "Decisions"). Running,
// stopped, killed and restarted, it leaves nothing beside its log but what it
// always has, and a second server started on the log while the first runs
// starts, as it always has.
func TestTheServerTakesNoLockBesideItsLog(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "keel.aof")
	args := []string{"-appendonly", "-appendfsync", "always", "-appendfilename", path}
	noLock := func(when string) {
		t.Helper()
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".lock") {
				t.Fatalf("%s: the server left %s beside its log", when, entry.Name())
			}
		}
	}

	first := startTestServer(t, args...)
	c, r := connectTest(t, first)
	expectCall(t, c, r, "+OK", "SET", "k", "first")
	noLock("while it runs")
	second := startTestServer(t, args...)
	noLock("with a second server on the same log")
	second.stop(t)
	first.stop(t)
	noLock("once it has stopped")

	again := startTestServer(t, args...)
	c, r = connectTest(t, again)
	expectCall(t, c, r, "first", "GET", "k")
	again.crash(t)
	noLock("once it has been killed")
}
