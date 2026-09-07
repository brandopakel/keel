package main

import (
	"io"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDeepPipelineProgressOrderAndRestart(t *testing.T) {
	for _, threads := range []string{"1", "4"} {
		for _, mode := range []string{"off", "barrier", "concurrent"} {
			t.Run(threads+"/"+mode, func(t *testing.T) {
				args := []string{"-io-threads", threads}
				if mode != "off" {
					args = append(args, "-appendonly", "-aof-async-append", "-appendfsync", "always", "-appendfilename", filepath.Join(t.TempDir(), "log"))
					if mode == "concurrent" {
						args = append(args, "-aof-concurrent-append")
					}
				}
				s := startTestServer(t, args...)
				c, r := connectTest(t, s)
				other, reader := connectTest(t, s)
				const count = 1025
				// All bytes arrive in one write. Draining must not depend on another
				// request waking the same socket after each bounded execution turn.
				if _, err := io.WriteString(c, strings.Repeat(request("INCR", "counter"), count)); err != nil {
					t.Fatal(err)
				}
				for i := 1; i <= count; i++ {
					if got := reply(t, r); got != ":"+strconv.Itoa(i) {
						t.Fatalf("reply %d: %q", i, got)
					}
					if i%127 == 0 {
						if got := call(t, other, reader, "PING"); got != "+PONG" {
							t.Fatal(got)
						}
					}
				}
				if got := call(t, c, r, "GET", "counter"); got != strconv.Itoa(count) {
					t.Fatal(got)
				}
				c.Close()
				other.Close()
				s.stop(t)
				if mode == "off" {
					return
				}
				for restart := 0; restart < 2; restart++ {
					s = startTestServer(t, args...)
					c, r = connectTest(t, s)
					if got := call(t, c, r, "GET", "counter"); got != strconv.Itoa(count) {
						t.Fatal(got)
					}
					c.Close()
					s.stop(t)
				}
			})
		}
	}
}

// A separate process test proves that another client executes while pipeline
// commands remain pending. The ordering/restart test above does not establish
// that overlap: its small replies may already be buffered in the client.
func TestPipelineYieldsWithCommandsStillPending(t *testing.T) {
	for _, threads := range []string{"1", "4"} {
		for _, mode := range []string{"off", "barrier", "concurrent"} {
			t.Run(threads+"/"+mode, func(t *testing.T) {
				args := []string{"-io-threads", threads}
				if mode != "off" {
					args = append(args, "-appendonly", "-aof-async-append", "-appendfsync", "always", "-appendfilename", filepath.Join(t.TempDir(), "log"))
					if mode == "concurrent" {
						args = append(args, "-aof-concurrent-append")
					}
				}
				s := startTestServer(t, args...)
				other, reader := connectTest(t, s)
				if got := call(t, other, reader, "SET", "large", strings.Repeat("x", 1<<20)); got != "+OK" {
					t.Fatal(got)
				}
				c, r := connectTest(t, s)
				if err := c.(*idleConn).Conn.(*net.TCPConn).SetReadBuffer(1024); err != nil {
					t.Fatal(err)
				}
				const count = 33
				pipeline := strings.Repeat(request("INCR", "counter")+request("GET", "large"), count)
				if _, err := io.WriteString(c, pipeline); err != nil {
					t.Fatal(err)
				}
				if got := reply(t, r); got != ":1" {
					t.Fatal(got)
				}
				// Leave the bulk replies unread, then observe actual backpressure.
				observed := false
				deadline := time.Now().Add(2 * time.Second)
				for time.Now().Before(deadline) {
					stats := call(t, other, reader, "INFO", "clients")
					for _, line := range strings.Split(stats, "\r\n") {
						if raw, ok := strings.CutPrefix(line, "retained_reply_bytes:"); ok {
							queued, err := strconv.Atoi(raw)
							if err != nil {
								t.Fatal(err)
							}
							observed = queued > 64<<10
						}
					}
					if observed {
						break
					}
					time.Sleep(time.Millisecond)
				}
				if !observed {
					t.Fatal("pipeline never retained queued replies")
				}
				pendingAt, err := strconv.Atoi(call(t, other, reader, "GET", "counter"))
				if err != nil || pendingAt < 1 || pendingAt >= count {
					t.Fatalf("expected unfinished commands, counter=%d err=%v", pendingAt, err)
				}
				if got := call(t, other, reader, "PING"); got != "+PONG" {
					t.Fatal(got)
				}
				if err := c.Close(); err != nil {
					t.Fatal(err)
				}
				if err := other.Close(); err != nil {
					t.Fatal(err)
				}
				s.stop(t)
			})
		}
	}
}
