package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestASecondServerOnALogRefusesToStart: a server holds the lock beside its
// log, so a second server started on the same log refuses, with the line a
// failed startup prints and status 1, and the first goes on serving. Once the
// first has stopped, cleanly or killed, the log is free again: the kernel
// released the killed one's lock, and the lock file it left is not a stale
// lock.
func TestASecondServerOnALogRefusesToStart(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "keel.aof")
	args := []string{"-appendonly", "-appendfsync", "always", "-appendfilename", path}
	first := startTestServer(t, args...)
	c, r := connectTest(t, first)
	expectCall(t, c, r, "+OK", "SET", "k", "first")

	out := refusedTestServer(t, args...)
	want := "appendonly: log in use by another instance: " + path + ".lock\n"
	if !strings.HasSuffix(out, want) {
		t.Fatalf("the second server printed\n%s\nwhich does not end with %q", out, want)
	}
	expectCall(t, c, r, "first", "GET", "k")
	first.stop(t)

	second := startTestServer(t, args...)
	c, r = connectTest(t, second)
	expectCall(t, c, r, "first", "GET", "k")
	expectCall(t, c, r, "+OK", "SET", "k", "second")
	second.crash(t)

	third := startTestServer(t, args...)
	c, r = connectTest(t, third)
	expectCall(t, c, r, "second", "GET", "k")
	third.stop(t)
}
