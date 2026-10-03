package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// readValue reads one whole reply, arrays included, and renders it as text a
// test can compare: statuses and integers as their line, errors with their
// leading '-', bulk strings as their bytes, nil as (nil), and an array as its
// elements in brackets.
func readValue(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimSuffix(line, "\r\n")
	switch {
	case line == "$-1" || line == "*-1":
		return "(nil)", nil
	case strings.HasPrefix(line, "$"):
		n, err := strconv.Atoi(line[1:])
		if err != nil {
			return "", err
		}
		p := make([]byte, n+2)
		if _, err := io.ReadFull(r, p); err != nil {
			return "", err
		}
		return string(p[:n]), nil
	case strings.HasPrefix(line, "*"):
		n, err := strconv.Atoi(line[1:])
		if err != nil {
			return "", err
		}
		parts := make([]string, n)
		for i := range parts {
			if parts[i], err = readValue(r); err != nil {
				return "", err
			}
		}
		return "[" + strings.Join(parts, " | ") + "]", nil
	}
	return line, nil
}

// pipeline writes every command in one write, then reads one reply for each.
func pipeline(t *testing.T, c net.Conn, r *bufio.Reader, commands ...[]string) []string {
	t.Helper()
	var b strings.Builder
	for _, parts := range commands {
		b.WriteString(request(parts...))
	}
	if _, err := io.WriteString(c, b.String()); err != nil {
		t.Fatal(err)
	}
	replies := make([]string, len(commands))
	for i := range replies {
		var err error
		if replies[i], err = readValue(r); err != nil {
			t.Fatal(err)
		}
	}
	return replies
}

func expectReplies(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("replies:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

const execAbort = "-EXECABORT Transaction discarded because of previous errors."

// persistenceModes are the AOF configurations a transaction has to survive a
// restart under: each fsync policy with synchronous appends, and worker and
// concurrent appends under always, which defer replies to the worker.
var persistenceModes = map[string][]string{
	"always":     {"-appendfsync", "always"},
	"everysec":   {"-appendfsync", "everysec"},
	"no":         {"-appendfsync", "no"},
	"async":      {"-appendfsync", "always", "-aof-async-append"},
	"concurrent": {"-appendfsync", "always", "-aof-async-append", "-aof-concurrent-append"},
}

func TestTransactionsOverTheWireAndAcrossRestarts(t *testing.T) {
	for mode, policy := range persistenceModes {
		t.Run(mode, func(t *testing.T) {
			args := append([]string{"-appendonly", "-appendfilename", filepath.Join(t.TempDir(), "tx.aof")}, policy...)
			s := startTestServer(t, args...)
			c, r := connectTest(t, s)
			call(t, c, r, "SET", "text", "not a number")
			// The whole transaction in one write, as client libraries send it.
			binary := "\x00\xff\r\n*1\r\n$4\r\nEXEC\r\n"
			expectReplies(t, pipeline(t, c, r,
				[]string{"MULTI"}, []string{"SET", "counter", "1"}, []string{"INCR", "counter"},
				[]string{"INCR", "text"}, []string{"SET", "applied", "yes"}, []string{"GET", "counter"},
				[]string{"SET", "binary", binary}, []string{"GET", "binary"},
				[]string{"EXEC"}, []string{"GET", "applied"}),
				"+OK", "+QUEUED", "+QUEUED", "+QUEUED", "+QUEUED", "+QUEUED", "+QUEUED", "+QUEUED",
				"[+OK | :2 | -ERR value is not an integer or out of range | +OK | 2 | +OK | "+binary+"]", "yes")
			expectReplies(t, pipeline(t, c, r,
				[]string{"MULTI"}, []string{"SET", "aborted", "v"}, []string{"NOSUCHCOMMAND"},
				[]string{"EXEC"}, []string{"GET", "aborted"}),
				"+OK", "+QUEUED", "-ERR unknown command 'NOSUCHCOMMAND'", execAbort, "(nil)")
			expectReplies(t, pipeline(t, c, r,
				[]string{"MULTI"}, []string{"SET", "discarded", "v"}, []string{"DISCARD"},
				[]string{"EXEC"}, []string{"DISCARD"}, []string{"GET", "discarded"}),
				"+OK", "+QUEUED", "+OK", "-ERR EXEC without MULTI", "-ERR DISCARD without MULTI", "(nil)")
			expectReplies(t, pipeline(t, c, r,
				[]string{"MULTI"}, []string{"MULTI"}, []string{"GET", "applied"}, []string{"EXEC"}),
				"+OK", "-ERR MULTI calls can not be nested", "+QUEUED", "[yes]")
			c.Close()
			s.stop(t)
			for restart := 0; restart < 2; restart++ {
				s = startTestServer(t, args...)
				c, r = connectTest(t, s)
				for key, want := range map[string]string{"counter": "2", "applied": "yes", "binary": binary, "aborted": "", "discarded": ""} {
					if got := call(t, c, r, "GET", key); got != want && !(want == "" && got == "$-1") {
						t.Fatalf("restart %d: %s = %q, want %q", restart, key, got, want)
					}
				}
				c.Close()
				s.stop(t)
			}
		})
	}
}

// TestTransactionDisconnectDiscardsTheQueue: a connection that closes inside a
// transaction runs nothing it queued, and the memory it held is released.
func TestTransactionDisconnectDiscardsTheQueue(t *testing.T) {
	s := startTestServer(t)
	observer, or := connectTest(t, s)
	c, r := connectTest(t, s)
	expectReplies(t, pipeline(t, c, r, []string{"MULTI"}, []string{"SET", "k", strings.Repeat("v", 4<<20)}),
		"+OK", "+QUEUED")
	if retained := retainedInput(t, observer, or); retained < 4<<20 {
		t.Fatalf("queued command not retained as input: %d bytes", retained)
	}
	c.Close()
	deadline := time.Now().Add(3 * time.Second)
	for retainedInput(t, observer, or) >= 4<<20 {
		if time.Now().After(deadline) {
			t.Fatal("closed connection's queue was not released")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := call(t, observer, or, "EXISTS", "k"); got != ":0" {
		t.Fatalf("EXISTS after disconnect = %s", got)
	}
	s.stop(t)
}

func retainedInput(t *testing.T, c net.Conn, r *bufio.Reader) int {
	t.Helper()
	for _, line := range strings.Split(call(t, c, r, "INFO", "clients"), "\r\n") {
		if raw, ok := strings.CutPrefix(line, "retained_input_bytes:"); ok {
			n, err := strconv.Atoi(raw)
			if err != nil {
				t.Fatal(err)
			}
			return n
		}
	}
	t.Fatal("INFO clients has no retained_input_bytes")
	return 0
}

// TestTransactionIsAtomicAgainstAConcurrentClient keeps two counters equal
// inside every transaction while another connection reads them both as fast
// as it can. A reader seeing them differ saw half a transaction.
func TestTransactionIsAtomicAgainstAConcurrentClient(t *testing.T) {
	modes := map[string][]string{
		"memory":     nil,
		"io-threads": {"-io-threads", "4"},
		"everysec":   {"-appendonly", "-appendfsync", "everysec"},
		"concurrent": {"-appendonly", "-appendfsync", "always", "-aof-async-append", "-aof-concurrent-append"},
	}
	for mode, extra := range modes {
		t.Run(mode, func(t *testing.T) {
			args := append([]string(nil), extra...)
			if len(extra) > 0 && extra[0] == "-appendonly" {
				args = append(args, "-appendfilename", filepath.Join(t.TempDir(), "atomic.aof"))
			}
			s := startTestServer(t, args...)
			writer, wr := connectTest(t, s)
			var stop atomic.Bool
			var reads atomic.Int64
			failure := make(chan error, 4)
			var wg sync.WaitGroup
			for i := 0; i < 3; i++ {
				reader, rr := connectTest(t, s)
				wg.Add(1)
				go func() {
					defer wg.Done()
					for !stop.Load() {
						got, err := rpc(reader, rr, "MGET", "left", "right")
						if err != nil {
							failure <- err
							return
						}
						if got != "*2" {
							failure <- fmt.Errorf("MGET header %q", got)
							return
						}
						left, err1 := readReply(rr)
						right, err2 := readReply(rr)
						if err1 != nil || err2 != nil {
							failure <- fmt.Errorf("MGET body: %v %v", err1, err2)
							return
						}
						if left != right {
							failure <- fmt.Errorf("read half a transaction: left=%s right=%s", left, right)
							return
						}
						reads.Add(1)
					}
				}()
			}
			// Each transaction spans two writes and two runs of the event loop, so
			// the readers have every chance to run between its halves: a queued
			// command that ran early would show up as soon as the first half did.
			const rounds = 1000
			for i := 1; i <= rounds; i++ {
				expectReplies(t, pipeline(t, writer, wr, []string{"MULTI"}, []string{"INCR", "left"}), "+OK", "+QUEUED")
				got := pipeline(t, writer, wr, []string{"SET", "padding", strings.Repeat("p", 1024)},
					[]string{"INCR", "right"}, []string{"EXEC"})
				n := strconv.Itoa(i)
				expectReplies(t, got, "+QUEUED", "+QUEUED", "[:"+n+" | +OK | :"+n+"]")
			}
			stop.Store(true)
			wg.Wait()
			select {
			case err := <-failure:
				t.Fatal(err)
			default:
			}
			if reads.Load() == 0 {
				t.Fatal("the readers never overlapped the writer")
			}
			s.stop(t)
		})
	}
}

// TestTransactionTornTailIsCutBeforeItsMULTI writes a block whose EXEC never
// reached the disk, as a crash part way through appending one would, behind
// two complete transactions. Startup keeps both, sets the whole open block
// aside, and the log is usable afterwards.
func TestTransactionTornTailIsCutBeforeItsMULTI(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "torn.aof")
	args := []string{"-appendonly", "-appendfsync", "always", "-appendfilename", path}
	s := startTestServer(t, args...)
	c, r := connectTest(t, s)
	pipeline(t, c, r, []string{"MULTI"}, []string{"SET", "a", "1"}, []string{"SET", "b", "1"}, []string{"EXEC"})
	pipeline(t, c, r, []string{"MULTI"}, []string{"INCR", "a"}, []string{"INCR", "b"}, []string{"EXEC"})
	c.Close()
	// A crash, not a stop: under always every acknowledged block is on disk.
	s.cmd.Process.Kill()
	s.cmd.Wait()
	s.stopped = true
	intact, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	open := "*1\r\n$5\r\nMULTI\r\n" + request("SET", "a", "torn") + request("SET", "b", "torn")[:9]
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(open)
	f.Close()
	for restart := 0; restart < 2; restart++ {
		s = startTestServer(t, args...)
		c, r = connectTest(t, s)
		if a, b := call(t, c, r, "GET", "a"), call(t, c, r, "GET", "b"); a != "2" || b != "2" {
			t.Fatalf("restart %d: a=%s b=%s", restart, a, b)
		}
		if restart == 0 {
			if body, _ := os.ReadFile(path); string(body) != string(intact) {
				t.Fatal("the log was not cut back to before the open block")
			}
			backups, _ := filepath.Glob(filepath.Join(dir, ".keel-torn-tail-*"))
			if len(backups) != 1 {
				t.Fatalf("torn tail backups: %v", backups)
			}
			if saved, _ := os.ReadFile(backups[0]); string(saved) != open {
				t.Fatalf("backup holds %q", saved)
			}
			pipeline(t, c, r, []string{"MULTI"}, []string{"INCR", "a"}, []string{"DECR", "a"}, []string{"EXEC"})
		}
		c.Close()
		s.stop(t)
	}
}

// TestTransactionReachesAReplicaWhole runs the same check against a replica,
// over both replication protocols: transactions keep two keys equal on the
// primary, and a reader on the replica must never see them differ. The
// replica also refuses a transaction that writes.
func TestTransactionReachesAReplicaWhole(t *testing.T) {
	for _, protocol := range []string{"1", "2"} {
		t.Run("protocol-"+protocol, func(t *testing.T) {
			primary := startTestServer(t, "-appendonly", "-replication-protocol", protocol, "-replication-feed",
				"-requirepass-env", "KEEL_TEST_PASSWORD", "-appendfilename", filepath.Join(t.TempDir(), "primary"))
			pc, pr := connectTest(t, primary)
			call(t, pc, pr, "AUTH", "integration-secret")
			call(t, pc, pr, "MSET", "left", "0", "right", "0")
			replica := startTestServer(t, "-appendonly", "-replication-protocol", protocol,
				"-requirepass-env", "KEEL_TEST_PASSWORD", "-primary-password-env", "KEEL_TEST_PASSWORD",
				"-replicaof", primary.addr, "-appendfilename", filepath.Join(t.TempDir(), "replica"))
			rc, rr := connectTest(t, replica)
			call(t, rc, rr, "AUTH", "integration-secret")
			deadline := time.Now().Add(10 * time.Second)
			for call(t, rc, rr, "GET", "left") != "0" {
				if time.Now().After(deadline) {
					t.Fatal("replica did not synchronise")
				}
				time.Sleep(25 * time.Millisecond)
			}
			expectReplies(t, pipeline(t, rc, rr, []string{"MULTI"}, []string{"SET", "left", "local"},
				[]string{"GET", "left"}, []string{"EXEC"}),
				"+OK", "-READONLY You can't write against a read only replica.", "+QUEUED", execAbort)

			const rounds = 200
			done := make(chan error, 1)
			go func() {
				for i := 1; i <= rounds; i++ {
					// Two writes, so the primary's loop turns between the halves.
					if _, err := rpcAll(pc, pr, request("MULTI")+request("INCR", "left"), 2); err != nil {
						done <- err
						return
					}
					got, err := rpcAll(pc, pr, request("SET", "padding", strings.Repeat("p", 4096))+
						request("INCR", "right")+request("EXEC"), 3)
					if err != nil {
						done <- err
						return
					}
					n := strconv.Itoa(i)
					if got[2] != "[:"+n+" | +OK | :"+n+"]" {
						done <- fmt.Errorf("EXEC %d answered %s", i, got[2])
						return
					}
				}
				done <- nil
			}()
			final := strconv.Itoa(rounds)
			deadline = time.Now().Add(20 * time.Second)
			for {
				got := pipeline(t, rc, rr, []string{"MULTI"}, []string{"GET", "left"}, []string{"GET", "right"}, []string{"EXEC"})
				if time.Now().After(deadline) {
					t.Fatalf("replica did not reach %s: %q", final, got)
				}
				if strings.HasPrefix(got[3], "-") {
					// A replica refuses reads, queued or at EXEC, for as long as
					// it has no recent primary state.
					if !strings.Contains(strings.Join(got, " "), "MASTERDOWN") && got[3] != execAbort {
						t.Fatalf("replica EXEC answered %q", got)
					}
					time.Sleep(10 * time.Millisecond)
					continue
				}
				values := strings.Split(strings.TrimSuffix(strings.TrimPrefix(got[3], "["), "]"), " | ")
				if len(values) != 2 {
					t.Fatalf("replica EXEC answered %q", got[3])
				}
				if values[0] != values[1] {
					t.Fatalf("replica applied half a transaction: left=%s right=%s", values[0], values[1])
				}
				if values[0] == final {
					break
				}
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			rc.Close()
			replica.stop(t)
			pc.Close()
			primary.stop(t)
		})
	}
}

// rpcAll writes one pipelined request and reads n whole replies.
func rpcAll(c net.Conn, r *bufio.Reader, requests string, n int) ([]string, error) {
	if _, err := io.WriteString(c, requests); err != nil {
		return nil, err
	}
	replies := make([]string, n)
	for i := range replies {
		var err error
		if replies[i], err = readValue(r); err != nil {
			return nil, err
		}
	}
	return replies, nil
}
