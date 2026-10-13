package main

import (
	"fmt"
	"math"
	"os"
	"strconv"
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

// hostSomaxconn is the host's somaxconn as the server reads it, and whether
// it could be read: only Linux has it, and only there does the server warn.
func hostSomaxconn() (int, bool) {
	body, err := os.ReadFile("/proc/sys/net/core/somaxconn")
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(body)))
	return n, err == nil && n > 0
}

// TestTCPBacklogIsItsOwnSetting: the listen backlog is -tcp-backlog, Redis's
// tcp-backlog, 511 by default, and no longer -maxclients; CONFIG GET reports
// the value the server listens with. Where the host has a somaxconn (Linux),
// a backlog above it is warned about at startup, in Redis's words, and the
// server listens anyway, capped by the kernel; one within it is not.
func TestTCPBacklogIsItsOwnSetting(t *testing.T) {
	t.Parallel()
	somaxconn, limited := hostSomaxconn()
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
	if logged := s.log.String(); (!limited || somaxconn >= 7) && strings.Contains(logged, "WARNING: The TCP backlog") {
		t.Fatalf("a backlog within somaxconn (%d, read %v) was warned about:\n%s", somaxconn, limited, logged)
	}

	// Ask for one more than the host allows, so the warning is due whatever
	// the host's limit is.
	if !limited || somaxconn >= math.MaxInt32 {
		t.Logf("no somaxconn below the largest backlog to exceed (%d, read %v); the warning is Linux's", somaxconn, limited)
		return
	}
	over := strconv.Itoa(somaxconn + 1)
	s = startTestServer(t, "-tcp-backlog", over)
	if got := configGet(t, s, "tcp-backlog"); got != "[tcp-backlog | "+over+"]" {
		t.Fatalf("asked for: %s", got)
	}
	s.stop(t)
	want := fmt.Sprintf("WARNING: The TCP backlog setting of %s cannot be enforced because /proc/sys/net/core/somaxconn is set to the lower value of %d.", over, somaxconn)
	if logged := s.log.String(); !strings.Contains(logged, want) {
		t.Fatalf("want Redis's warning logged:\n%s", logged)
	}
}
