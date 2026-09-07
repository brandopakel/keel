package main

import (
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
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
