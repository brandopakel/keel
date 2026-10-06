package main

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A rewrite whose own write fails must not stop the server, as Redis's does
// not. These run the real server under a per-file limit (RLIMIT_FSIZE), so the
// failure is the kernel's EFBIG and not an injected one. The key that trips it
// is a Bloom filter: its log record is one short BF.RESERVE, while a rewrite
// writes its whole image, several MiB. So the log stays far below the limit
// and the rewrite's file passes it.
const rewriteFileLimit = "1048576"

var bigFilter = []string{"BF.RESERVE", "big", "0.0001", "1000000"}

func persistenceInfo(t *testing.T, c net.Conn, r *bufio.Reader) map[string]string {
	t.Helper()
	fields := map[string]string{}
	for _, line := range strings.Split(call(t, c, r, "INFO", "persistence"), "\r\n") {
		if name, value, ok := strings.Cut(line, ":"); ok {
			fields[name] = value
		}
	}
	return fields
}

// awaitRewriteIdle waits until no rewrite is running or scheduled.
func awaitRewriteIdle(t *testing.T, c net.Conn, r *bufio.Reader) map[string]string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		info := persistenceInfo(t, c, r)
		if info["aof_rewrite_in_progress"] == "0" && info["aof_rewrite_scheduled"] == "0" {
			return info
		}
		if time.Now().After(deadline) {
			t.Fatalf("rewrite still running: %v", info)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func expectInfo(t *testing.T, info map[string]string, want map[string]string) {
	t.Helper()
	for name, value := range want {
		if info[name] != value {
			t.Fatalf("INFO persistence %s=%q, want %q (all: %v)", name, info[name], value, info)
		}
	}
}

func expectCall(t *testing.T, c net.Conn, r *bufio.Reader, want string, parts ...string) {
	t.Helper()
	if got := call(t, c, r, parts...); got != want {
		t.Fatalf("%v answered %q, want %q", parts, got, want)
	}
}

// crash stops the server as a power cut would, without its clean shutdown.
func (s *testServer) crash(t *testing.T) {
	t.Helper()
	_ = s.cmd.Process.Kill()
	_ = s.cmd.Wait()
	s.stopped = true
}

func addBigFilter(t *testing.T, c net.Conn, r *bufio.Reader, path string) {
	t.Helper()
	expectCall(t, c, r, "+OK", bigFilter...)
	usage, err := strconv.Atoi(strings.TrimPrefix(call(t, c, r, "MEMORY", "USAGE", "big"), ":"))
	if err != nil || usage < 2<<20 {
		t.Fatalf("the filter must be well past the 1 MiB file limit: %d %v", usage, err)
	}
	if info, err := os.Stat(path); err != nil || info.Size() > 64<<10 {
		t.Fatalf("the log must stay far below it: %v %v", info, err)
	}
}

func TestServerKeepsServingAfterAFailedRewrite(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"always", "everysec", "async", "concurrent"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "store.aof")
			args := append([]string{"-appendonly", "-appendfilename", path, "-auto-aof-rewrite-percentage", "0"}, persistenceModes[mode]...)
			s := startLimitedTestServer(t, rewriteFileLimit, args...)
			c, r := connectTest(t, s)
			expectCall(t, c, r, "+OK", "SET", "before", "1")
			addBigFilter(t, c, r, path)

			expectCall(t, c, r, "+Background append only file rewriting started", "BGREWRITEAOF")
			// A transaction goes into the log while the rewrite is failing.
			expectReplies(t, pipeline(t, c, r, []string{"MULTI"}, []string{"SET", "tx-a", "1"}, []string{"SET", "tx-b", "1"}, []string{"EXEC"}),
				"+OK", "+QUEUED", "+QUEUED", "[+OK | +OK]")
			expectInfo(t, awaitRewriteIdle(t, c, r), map[string]string{
				"aof_last_bgrewrite_status": "err", "aof_rewrites_consecutive_failures": "1",
				"aof_rewrites": "0", "aof_last_write_status": "ok", "aof_current_rewrite_time_sec": "-1",
			})
			if _, err := os.Stat(path + ".rewrite"); !os.IsNotExist(err) {
				t.Fatalf("the temporary file is still there: %v", err)
			}

			// Still serving, and still logging writes.
			expectCall(t, c, r, "+OK", "SET", "after", "2")
			expectCall(t, c, r, ":1", "INCR", "counter")
			expectCall(t, c, r, "2", "GET", "after")
			// BGREWRITEAOF after a failure starts again, as in Redis, and fails again.
			expectCall(t, c, r, "+Background append only file rewriting started", "BGREWRITEAOF")
			expectInfo(t, awaitRewriteIdle(t, c, r), map[string]string{
				"aof_last_bgrewrite_status": "err", "aof_rewrites_consecutive_failures": "2",
			})
			expectCall(t, c, r, ":2", "INCR", "counter")
			c.Close()

			// Under always an acknowledged write is on disk, so a crash keeps
			// it; everysec promises only a clean stop.
			if mode == "everysec" {
				s.stop(t)
			} else {
				s.crash(t)
			}
			logged := s.log.String()
			for _, want := range []string{"Background AOF rewrite terminated with error: ", "file too large"} {
				if strings.Count(logged, want) != 2 {
					t.Fatalf("want %q logged for each failure:\n%s", want, logged)
				}
			}
			if strings.Contains(logged, "write failed, stopping") {
				t.Fatalf("a failed rewrite stopped the server:\n%s", logged)
			}

			s = startLimitedTestServer(t, rewriteFileLimit, args...)
			c, r = connectTest(t, s)
			for key, value := range map[string]string{"before": "1", "after": "2", "counter": "2", "tx-a": "1", "tx-b": "1"} {
				expectCall(t, c, r, value, "GET", key)
			}
			expectCall(t, c, r, ":1", "EXISTS", "big")
			expectInfo(t, persistenceInfo(t, c, r), map[string]string{"aof_last_bgrewrite_status": "ok", "aof_rewrites_consecutive_failures": "0"})

			// A later rewrite works once nothing it writes passes the limit.
			expectCall(t, c, r, ":1", "DEL", "big")
			expectCall(t, c, r, "+Background append only file rewriting started", "BGREWRITEAOF")
			expectInfo(t, awaitRewriteIdle(t, c, r), map[string]string{
				"aof_last_bgrewrite_status": "ok", "aof_rewrites_consecutive_failures": "0", "aof_rewrites": "1",
			})
			expectCall(t, c, r, "+OK", "SET", "final", "3")
			c.Close()
			s.stop(t)
			s = startLimitedTestServer(t, rewriteFileLimit, args...)
			c, r = connectTest(t, s)
			for key, value := range map[string]string{"before": "1", "after": "2", "counter": "2", "tx-a": "1", "tx-b": "1", "final": "3"} {
				expectCall(t, c, r, value, "GET", key)
			}
			expectCall(t, c, r, ":0", "EXISTS", "big")
			c.Close()
			s.stop(t)
		})
	}
}

// Automatic rewrites back off as Redis's do: three attempts, then the limit,
// and no attempt in the minute after it however many writes arrive.
func TestAutomaticRewriteBacksOffOnAFailingDisk(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "store.aof")
	args := []string{"-appendonly", "-appendfsync", "always", "-appendfilename", path,
		"-auto-aof-rewrite-percentage", "100", "-auto-aof-rewrite-min-size", "1kb"}
	s := startLimitedTestServer(t, rewriteFileLimit, args...)
	c, r := connectTest(t, s)
	addBigFilter(t, c, r, path)
	writes := 0
	write := func() {
		t.Helper()
		writes++
		expectCall(t, c, r, "+OK", "SET", "k"+strconv.Itoa(writes%50), strconv.Itoa(writes))
	}
	deadline := time.Now().Add(10 * time.Second)
	for persistenceInfo(t, c, r)["aof_rewrites_consecutive_failures"] != "3" {
		if time.Now().After(deadline) {
			t.Fatalf("three automatic attempts did not fail: %v", persistenceInfo(t, c, r))
		}
		write()
	}
	// A second of steady writes after the limit starts nothing.
	for until := time.Now().Add(time.Second); time.Now().Before(until); {
		write()
	}
	expectInfo(t, persistenceInfo(t, c, r), map[string]string{
		"aof_rewrites_consecutive_failures": "3", "aof_last_bgrewrite_status": "err", "aof_rewrite_in_progress": "0",
	})
	// BGREWRITEAOF is not held back by the limit.
	expectCall(t, c, r, "+Background append only file rewriting started", "BGREWRITEAOF")
	expectInfo(t, awaitRewriteIdle(t, c, r), map[string]string{"aof_rewrites_consecutive_failures": "4"})
	write()
	c.Close()
	s.crash(t)
	logged := s.log.String()
	for want, n := range map[string]int{
		"Starting automatic rewriting of AOF on":                                                        3,
		"Background AOF rewrite terminated with error: ":                                                4,
		"Background AOF rewrite has repeatedly failed and triggered the limit, will retry in 1 minutes": 1,
	} {
		if got := strings.Count(logged, want); got != n {
			t.Fatalf("%q logged %d times, want %d:\n%s", want, got, n, logged)
		}
	}
	s = startLimitedTestServer(t, rewriteFileLimit, args...)
	c, r = connectTest(t, s)
	expectCall(t, c, r, strconv.Itoa(writes), "GET", "k"+strconv.Itoa(writes%50))
	c.Close()
	s.stop(t)
}

// Replicas over both protocols go on receiving writes, transactions included,
// while the primary's rewrites fail. A protocol 1 replica joining meanwhile
// syncs from key images and needs no rewrite. A protocol 2 replica joining
// needs a snapshot, which is a rewrite: its pulls are paced by the same limit
// as automatic rewrites, and the first rewrite that works serves it.
func TestReplicasThroughAFailedRewrite(t *testing.T) {
	t.Parallel()
	for _, protocol := range []string{"1", "2"} {
		t.Run("protocol-"+protocol, func(t *testing.T) {
			t.Parallel()
			primaryPath := filepath.Join(t.TempDir(), "primary.aof")
			primary := startLimitedTestServer(t, rewriteFileLimit, "-appendonly", "-appendfsync", "always", "-replication-protocol", protocol, "-replication-feed",
				"-requirepass-env", "KEEL_TEST_PASSWORD", "-appendfilename", primaryPath, "-auto-aof-rewrite-percentage", "0")
			pc, pr := connectTest(t, primary)
			expectCall(t, pc, pr, "+OK", "AUTH", "integration-secret")
			expectCall(t, pc, pr, "+OK", "SET", "k", "before")
			startReplica := func() (net.Conn, *bufio.Reader, *testServer) {
				replica := startTestServer(t, "-appendonly", "-replication-protocol", protocol,
					"-requirepass-env", "KEEL_TEST_PASSWORD", "-primary-password-env", "KEEL_TEST_PASSWORD",
					"-replicaof", primary.addr, "-appendfilename", filepath.Join(t.TempDir(), "replica.aof"))
				rc, rr := connectTest(t, replica)
				expectCall(t, rc, rr, "+OK", "AUTH", "integration-secret")
				return rc, rr, replica
			}
			await := func(c net.Conn, r *bufio.Reader, want string, parts ...string) {
				t.Helper()
				deadline := time.Now().Add(15 * time.Second)
				for {
					got := pipeline(t, c, r, parts)[0]
					if got == want {
						return
					}
					if time.Now().After(deadline) {
						t.Fatalf("replica answered %v with %q, want %q", parts, got, want)
					}
					time.Sleep(25 * time.Millisecond)
				}
			}
			rc, rr, replica := startReplica()
			await(rc, rr, "before", "GET", "k")
			addBigFilter(t, pc, pr, primaryPath)
			await(rc, rr, ":1", "EXISTS", "big")

			expectCall(t, pc, pr, "+Background append only file rewriting started", "BGREWRITEAOF")
			expectInfo(t, awaitRewriteIdle(t, pc, pr), map[string]string{"aof_last_bgrewrite_status": "err"})
			expectCall(t, pc, pr, "+OK", "SET", "k", "after")
			expectReplies(t, pipeline(t, pc, pr, []string{"MULTI"}, []string{"SET", "left", "1"}, []string{"SET", "right", "1"}, []string{"EXEC"}),
				"+OK", "+QUEUED", "+QUEUED", "[+OK | +OK]")
			await(rc, rr, "after", "GET", "k")
			await(rc, rr, "[1 | 1]", "MGET", "left", "right")

			// A replica joining while rewrites fail.
			var jc net.Conn
			var jr *bufio.Reader
			var joined *testServer
			if protocol == "1" {
				jc, jr, joined = startReplica()
				await(jc, jr, "after", "GET", "k")
			} else {
				// A protocol 2 replica joining now would be served the
				// snapshot the first one synced from. Opaque commands publish
				// their whole image to the stream and one short record to the
				// log, so a few BF.ADDs roll the stream's history past that
				// snapshot, with the first replica keeping up throughout. The
				// replica joining then needs a new snapshot, which is a rewrite.
				for i := 0; i < 8; i++ {
					item := "item-" + strconv.Itoa(i)
					expectCall(t, pc, pr, ":1", "BF.ADD", "big", item)
					await(rc, rr, ":1", "BF.EXISTS", "big", item)
				}
				jc, jr, joined = startReplica()
				deadline := time.Now().Add(10 * time.Second)
				for persistenceInfo(t, pc, pr)["aof_rewrites_consecutive_failures"] != "3" {
					if time.Now().After(deadline) {
						t.Fatalf("the joining replica's snapshot rewrites did not fail: %v", persistenceInfo(t, pc, pr))
					}
					time.Sleep(25 * time.Millisecond)
				}
				// Its pulls go on, and start nothing more.
				time.Sleep(time.Second)
				expectInfo(t, persistenceInfo(t, pc, pr), map[string]string{"aof_rewrites_consecutive_failures": "3", "aof_rewrite_in_progress": "0"})
				expectCall(t, pc, pr, "+OK", "SET", "k", "after")
			}

			// The next rewrite that works, once nothing passes the limit.
			expectCall(t, pc, pr, ":1", "DEL", "big")
			expectCall(t, pc, pr, "+Background append only file rewriting started", "BGREWRITEAOF")
			expectInfo(t, awaitRewriteIdle(t, pc, pr), map[string]string{"aof_last_bgrewrite_status": "ok", "aof_rewrites_consecutive_failures": "0"})
			expectCall(t, pc, pr, "+OK", "SET", "k", "final")
			for _, c := range []struct {
				c net.Conn
				r *bufio.Reader
			}{{rc, rr}, {jc, jr}} {
				await(c.c, c.r, "final", "GET", "k")
				await(c.c, c.r, "[1 | 1]", "MGET", "left", "right")
				await(c.c, c.r, ":0", "EXISTS", "big")
			}
			rc.Close()
			jc.Close()
			replica.stop(t)
			joined.stop(t)
			pc.Close()
			primary.stop(t)
			logged := primary.log.String()
			failures := 1
			if protocol == "2" {
				failures = 3
				if strings.Count(logged, "triggered the limit, will retry in 1 minutes") != 1 {
					t.Fatalf("the snapshot rewrites were not limited:\n%s", logged)
				}
			}
			if got := strings.Count(logged, "Background AOF rewrite terminated with error: "); got != failures {
				t.Fatalf("%d failed rewrites logged, want %d:\n%s", got, failures, logged)
			}
		})
	}
}
