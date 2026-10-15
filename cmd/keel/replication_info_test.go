package main

import (
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// atLeast is whether field, an integer, is at least n.
func atLeast(field string, n int) bool {
	v, err := strconv.Atoi(field)
	return err == nil && v >= n
}

// infoSection reads INFO section from a connection as name: value fields.
func infoSection(t *testing.T, s *testServer, section string) map[string]string {
	t.Helper()
	c, r := connectTest(t, s)
	defer c.Close()
	call(t, c, r, "AUTH", "integration-secret")
	fields := map[string]string{}
	for _, line := range strings.Split(call(t, c, r, "INFO", section), "\r\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && !strings.HasPrefix(line, "#") {
			fields[k] = v
		}
	}
	return fields
}

// TestReplicationInfoInRedisTerms: through real servers, a primary reports
// its replica as Redis's connected_slaves and slave0 line and counts its full
// sync; the replica reports Redis's master_* link, up, with the primary's
// replication ID and offset; and the link goes down when the primary does.
func TestReplicationInfoInRedisTerms(t *testing.T) {
	t.Parallel()
	primary := startTestServer(t, "-appendonly", "-replication-feed", "-requirepass-env", "KEEL_TEST_PASSWORD",
		"-appendfilename", filepath.Join(t.TempDir(), "primary"))
	pc, pr := connectTest(t, primary)
	call(t, pc, pr, "AUTH", "integration-secret")
	call(t, pc, pr, "SET", "k", "v")
	replica := startTestServer(t, "-appendonly", "-requirepass-env", "KEEL_TEST_PASSWORD", "-primary-password-env", "KEEL_TEST_PASSWORD",
		"-replicaof", primary.addr, "-appendfilename", filepath.Join(t.TempDir(), "replica"))

	var p, r map[string]string
	deadline := time.Now().Add(10 * time.Second)
	for {
		p, r = infoSection(t, primary, "replication"), infoSection(t, replica, "replication")
		if p["connected_slaves"] == "1" && r["master_link_status"] == "up" && r["slave_repl_offset"] == p["master_repl_offset"] {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the replica did not come up: primary %v replica %v", p, r)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if p["role"] != "master" || !strings.HasPrefix(p["slave0"], "ip=127.0.0.1,port=") ||
		!strings.Contains(p["slave0"], ",state=online,offset=") || p["repl_backlog_active"] != "1" {
		t.Fatalf("primary: %v", p)
	}
	if stats := infoSection(t, primary, "stats"); !atLeast(stats["sync_full"], 1) {
		t.Fatalf("the replica's first sync is a full one: %v", stats["sync_full"])
	}
	host, port, _ := net.SplitHostPort(primary.addr)
	if r["role"] != "slave" || r["master_host"] != host || r["master_port"] != port || r["slave_read_only"] != "1" ||
		r["master_replid"] != p["master_replid"] || r["master_sync_in_progress"] != "0" || !atLeast(r["master_total_sync_attempts"], 1) {
		t.Fatalf("replica: %v (primary %v)", r, p)
	}

	pc.Close()
	primary.stop(t)
	deadline = time.Now().Add(10 * time.Second)
	for r = infoSection(t, replica, "replication"); r["master_link_status"] != "down"; r = infoSection(t, replica, "replication") {
		if time.Now().After(deadline) {
			t.Fatalf("the link stayed up with its primary gone: %v", r)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if r["master_last_io_seconds_ago"] != "-1" || !atLeast(r["master_link_down_since_seconds"], 0) {
		t.Fatalf("down: %v", r)
	}
	replica.stop(t)
}
