package main

import (
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A failed write of the log itself, as Redis 8.10.1 handles it. These run the
// real server under a per-file limit (RLIMIT_FSIZE), so that the failure is
// the kernel's EFBIG, "File too large", once the log reaches the limit.
const logFileLimit = "65536"

// killedValue is a value of about a kilobyte, so that the log reaches the
// limit after some sixty writes.
var killedValue = strings.Repeat("v", 1024)

// TestServerRefusesWritesWhileItsLogFails: under everysec and no, a server
// whose log can no longer be written keeps serving, as Redis does. The write
// whose record no longer fits is answered OK, as Redis answers it, since it
// ran before the failure was known; after it, write commands and PING are
// refused with Redis's MISCONF error, in Redis's words, reads are served, INFO
// says err, and a write queued in a transaction aborts it. The short write is
// cut back off the log, which stays whole. SIGTERM then stops the server with
// status 0, as Redis exits after logging a log it cannot flush, and the log it
// leaves opens with no repair and holds every write that reached it.
func TestServerRefusesWritesWhileItsLogFails(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"everysec", "no"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "store.aof")
			args := append([]string{"-appendonly", "-appendfilename", path, "-auto-aof-rewrite-percentage", "0"}, persistenceModes[mode]...)
			s := startLimitedTestServer(t, logFileLimit, args...)
			c, r := connectTest(t, s)
			const misconf = "-MISCONF Errors writing to the AOF file: File too large"
			inflight := -1
			for i := 0; inflight < 0; i++ {
				if i == 1000 {
					t.Fatal("the log never reached its limit")
				}
				switch got := call(t, c, r, "SET", "k"+strconv.Itoa(i), killedValue); got {
				case "+OK":
				case misconf:
					// The write before this one was in flight when the write
					// of its record failed.
					inflight = i - 1
				default:
					t.Fatalf("SET k%d answered %q", i, got)
				}
			}
			if inflight < 10 {
				t.Fatalf("the log failed after %d writes", inflight)
			}
			expectCall(t, c, r, killedValue, "GET", "k0")
			expectCall(t, c, r, misconf, "PING")
			expectCall(t, c, r, misconf, "INCR", "counter")
			expectInfo(t, persistenceInfo(t, c, r), map[string]string{"aof_last_write_status": "err"})
			expectReplies(t, pipeline(t, c, r, []string{"MULTI"}, []string{"SET", "queued", "v"}, []string{"EXEC"}),
				"+OK", misconf, execAbort)
			c.Close()

			s.stop(t)
			logged := s.log.String()
			for _, want := range []string{"appendonly: error writing to the log: ", "file too large", "appendonly: close failed, exiting anyway: "} {
				if !strings.Contains(logged, want) {
					t.Fatalf("want %q logged:\n%s", want, logged)
				}
			}
			if strings.Contains(logged, "write failed, stopping") {
				t.Fatalf("a failed write stopped the server:\n%s", logged)
			}

			s = startTestServer(t, args...)
			c, r = connectTest(t, s)
			for i := range inflight {
				expectCall(t, c, r, killedValue, "GET", "k"+strconv.Itoa(i))
			}
			// The write in flight was answered and never reached the log, as a
			// Redis whose write keeps failing loses it at its exit.
			expectCall(t, c, r, "$-1", "GET", "k"+strconv.Itoa(inflight))
			s.stop(t)
			if logged := s.log.String(); strings.Contains(logged, "truncated final command") {
				t.Fatalf("the log needed a repair:\n%s", logged)
			}
		})
	}
}

// TestServerStopsWhenItsLogFailsUnderAlways: under always Redis exits on a
// failed write, with status 1, and so does the server, as does its worker
// append, which is not retried. With synchronous appends the short write is
// cut back off the log first, as Redis truncates it before it exits, so the
// log opens with no repair; the worker's batch is not cut back, and its torn
// tail is repaired at the next start. Either way the log holds every write
// that was acknowledged.
func TestServerStopsWhenItsLogFailsUnderAlways(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"always", "async"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "store.aof")
			args := append([]string{"-appendonly", "-appendfilename", path, "-auto-aof-rewrite-percentage", "0"}, persistenceModes[mode]...)
			s := startLimitedTestServer(t, logFileLimit, args...)
			c, r := connectTest(t, s)
			acked := 0
			for ; ; acked++ {
				if acked == 1000 {
					t.Fatal("the log never reached its limit")
				}
				if _, err := io.WriteString(c, request("SET", "k"+strconv.Itoa(acked), killedValue)); err != nil {
					break
				}
				if got, err := readValue(r); err != nil {
					break
				} else if got != "+OK" {
					t.Fatalf("SET k%d answered %q", acked, got)
				}
			}
			done := make(chan error, 1)
			go func() { done <- s.cmd.Wait() }()
			select {
			case err := <-done:
				s.stopped = true
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 1 {
					t.Fatalf("the server exited with %v, want status 1:\n%s", err, s.log.String())
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the server did not stop")
			}
			if logged := s.log.String(); !strings.Contains(logged, "file too large") {
				t.Fatalf("want the failure logged:\n%s", logged)
			}

			s = startTestServer(t, args...)
			c, r = connectTest(t, s)
			for i := range acked {
				expectCall(t, c, r, killedValue, "GET", "k"+strconv.Itoa(i))
			}
			s.stop(t)
			if logged := s.log.String(); mode == "always" && strings.Contains(logged, "truncated final command") {
				t.Fatalf("the log needed a repair:\n%s", logged)
			}
		})
	}
}
