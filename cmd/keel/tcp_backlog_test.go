package main

import (
	"runtime"
	"strings"
	"testing"
)

// configGet answers one CONFIG GET as the server renders it: a flat array of
// name and value, which readValue prints as "[name | value]".
func configGet(t *testing.T, s *testServer, name string) string {
	t.Helper()
	c, r := connectTest(t, s)
	defer c.Close()
	return pipeline(t, c, r, []string{"CONFIG", "GET", name})[0]
}

// TestTCPBacklogIsItsOwnSetting: the listen backlog is -tcp-backlog, Redis's
// tcp-backlog, 511 by default, and no longer -maxclients; CONFIG GET reports
// the value the server listens with. On Linux, a backlog above the kernel's
// somaxconn is warned about at startup, in Redis's words, and the server
// listens anyway, capped by the kernel.
func TestTCPBacklogIsItsOwnSetting(t *testing.T) {
	t.Parallel()
	s := startTestServer(t)
	if got := configGet(t, s, "tcp-backlog"); got != "[tcp-backlog | 511]" {
		t.Fatalf("default: %s", got)
	}
	s.stop(t)

	s = startTestServer(t, "-tcp-backlog", "7", "-maxclients", "3")
	if got := configGet(t, s, "tcp-backlog"); got != "[tcp-backlog | 7]" {
		t.Fatalf("set: %s", got)
	}
	if got := configGet(t, s, "maxclients"); got != "[maxclients | 3]" {
		t.Fatalf("maxclients stays its own: %s", got)
	}
	s.stop(t)
	if logged := s.log.String(); strings.Contains(logged, "WARNING: The TCP backlog") {
		t.Fatalf("a backlog under somaxconn was warned about:\n%s", logged)
	}

	if runtime.GOOS != "linux" {
		return
	}
	s = startTestServer(t, "-tcp-backlog", "1000000")
	if got := configGet(t, s, "tcp-backlog"); got != "[tcp-backlog | 1000000]" {
		t.Fatalf("asked for: %s", got)
	}
	s.stop(t)
	const want = "WARNING: The TCP backlog setting of 1000000 cannot be enforced because /proc/sys/net/core/somaxconn is set to the lower value of "
	if logged := s.log.String(); !strings.Contains(logged, want) {
		t.Fatalf("want Redis's warning logged:\n%s", logged)
	}
}
