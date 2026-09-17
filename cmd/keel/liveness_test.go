package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestConcurrentAppendAnswersEveryRequest is the liveness check for the
// ordered-append path. Every shape of request that moves its state machine -
// admitted string runs, unmodelled runs that take the exclusive barrier, deep
// pipelines that yield and re-queue, a rewrite started under traffic, keys
// that expire and append on their own, unknown commands, a reply too large
// for the arena - from several connections at once, and every one of them
// answered within the per-operation deadline. INFO must then say the loop
// closed nobody for an unanswered request.
//
// A loop that forgets a connection fails this within the deadline, and the
// server log names the connection and its state (see sweepStalledClients).
// That is the check the 48-hour stall did not have.
func TestConcurrentAppendAnswersEveryRequest(t *testing.T) {
	s := startTestServer(t, "-appendonly", "-aof-async-append", "-aof-concurrent-append",
		"-appendfsync", "always", "-appendfilename", filepath.Join(t.TempDir(), "log"))
	const connections, rounds = 6, 120
	errs := make(chan error, connections)
	var wg sync.WaitGroup
	for n := 0; n < connections; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if err := hammer(s, n, rounds); err != nil {
				errs <- fmt.Errorf("connection %d: %w", n, err)
			}
		}(n)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	c, r := connectTest(t, s)
	info := call(t, c, r, "INFO", "clients")
	for _, field := range []string{"clients_closed_unanswered:0", "clients_closed_slow:0"} {
		if !strings.Contains(info, field) {
			t.Errorf("want %s in INFO clients:\n%s", field, info)
		}
	}
	if t.Failed() {
		t.Logf("server log:\n%s", s.log.String())
	}
}

// hammer is one connection's share of the traffic. It returns rather than
// failing the test because it runs off the test goroutine.
func hammer(s *testServer, n, rounds int) error {
	raw, err := net.DialTimeout("tcp", s.addr, time.Second)
	if err != nil {
		return err
	}
	defer raw.Close()
	conn := &idleConn{Conn: raw, idle: 5 * time.Second}
	r := bufio.NewReader(conn)
	key, expiring, counter, big := fmt.Sprintf("k%d", n), fmt.Sprintf("x%d", n), fmt.Sprintf("c%d", n), fmt.Sprintf("b%d", n)
	large := strings.Repeat("v", 256<<10)
	expect := func(got string, want string, parts ...string) error {
		if got != want {
			return fmt.Errorf("%s: got %q, want %q", strings.Join(parts, " "), got, want)
		}
		return nil
	}
	for i := 0; i < rounds; i++ {
		value := strconv.Itoa(i)
		got, err := rpc(conn, r, "SET", key, value)
		if err != nil {
			return err
		}
		if err := expect(got, "+OK", "SET", key); err != nil {
			return err
		}
		if got, err = rpc(conn, r, "GET", key); err != nil {
			return err
		}
		if err := expect(got, value, "GET", key); err != nil {
			return err
		}
		// Expires on its own a few milliseconds later, so the expiry cycle
		// appends a record with no client behind it.
		if got, err = rpc(conn, r, "SET", expiring, "x", "PX", "5"); err != nil {
			return err
		}
		if err := expect(got, "+OK", "SET", expiring); err != nil {
			return err
		}
		switch {
		case i%10 == 3:
			// Any answer is the point: started, already running, or waiting
			// for a pending append.
			if got, err = rpc(conn, r, "BGREWRITEAOF"); err != nil {
				return err
			}
			if !strings.HasPrefix(got, "+") && !strings.HasPrefix(got, "-ERR") {
				return fmt.Errorf("BGREWRITEAOF: unexpected %q", got)
			}
		case i%7 == 2:
			if got, err = rpc(conn, r, "INFO", "persistence"); err != nil {
				return err
			}
			if !strings.Contains(got, "aof_enabled:1") {
				return fmt.Errorf("INFO persistence: %q", got)
			}
		case i%5 == 1:
			if got, err = rpc(conn, r, "NOSUCHCOMMAND", key); err != nil {
				return err
			}
			if !strings.HasPrefix(got, "-ERR unknown command") {
				return fmt.Errorf("unknown command: %q", got)
			}
		case i%13 == 4:
			if got, err = rpc(conn, r, "DEL", key); err != nil {
				return err
			}
			if err := expect(got, ":1", "DEL", key); err != nil {
				return err
			}
		}
		if i%15 == 6 {
			// Deeper than one turn executes, so the remainder waits in the
			// fairness queue and resumes after the replies drain.
			var pipeline strings.Builder
			for j := 0; j < 100; j++ {
				pipeline.WriteString(request("INCR", counter))
			}
			if _, err := io.WriteString(conn, pipeline.String()); err != nil {
				return err
			}
			for j := 0; j < 100; j++ {
				if got, err = readReply(r); err != nil {
					return fmt.Errorf("pipelined INCR %d: %w", j, err)
				}
				if !strings.HasPrefix(got, ":") {
					return fmt.Errorf("pipelined INCR %d: %q", j, got)
				}
			}
		}
		if i%20 == 9 {
			if got, err = rpc(conn, r, "SET", big, large); err != nil {
				return err
			}
			if err := expect(got, "+OK", "SET", big); err != nil {
				return err
			}
			if got, err = rpc(conn, r, "GET", big); err != nil {
				return err
			}
			if len(got) != len(large) {
				return fmt.Errorf("GET %s: %d bytes, want %d", big, len(got), len(large))
			}
		}
	}
	return nil
}

func rpc(c net.Conn, r *bufio.Reader, parts ...string) (string, error) {
	if _, err := io.WriteString(c, request(parts...)); err != nil {
		return "", err
	}
	return readReply(r)
}

// readReply is reply without the test handle: bulk strings come back as their
// bytes, everything else as its line.
func readReply(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(line, "$") {
		n, err := strconv.Atoi(strings.TrimSpace(line[1:]))
		if err != nil {
			return "", err
		}
		if n >= 0 {
			p := make([]byte, n+2)
			if _, err := io.ReadFull(r, p); err != nil {
				return "", err
			}
			return string(p[:n]), nil
		}
	}
	return strings.TrimSpace(line), nil
}
