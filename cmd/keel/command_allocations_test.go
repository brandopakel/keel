package main

import (
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCommandReservationsRecoverAfterSlowReaders(t *testing.T) {
	s := startTestServer(t)
	c, reader := connectTest(t, s)
	large := strings.Repeat("x", 8<<20)
	if got := call(t, c, reader, "SET", "large", large); got != "+OK" {
		t.Fatal(got)
	}
	member := strings.Repeat("v", 1<<20)
	for i := 1; i <= 32; i++ {
		if got := call(t, c, reader, "RPUSH", "list", member); got != ":"+strconv.Itoa(i) {
			t.Fatal(got)
		}
	}
	var slow []net.Conn
	for i := 0; i < 24; i++ {
		conn, _ := connectTest(t, s)
		if err := conn.(*idleConn).Conn.(*net.TCPConn).SetReadBuffer(1024); err != nil {
			t.Fatal(err)
		}
		slow = append(slow, conn)
		if _, err := io.WriteString(conn, strings.Repeat(request("GET", "large"), 16)); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		if t.Failed() {
			s.captureFailure(t)
		}
	})
	stat := func(name string) int {
		t.Helper()
		info := call(t, c, reader, "INFO", "clients")
		for _, line := range strings.Split(info, "\r\n") {
			if value, ok := strings.CutPrefix(line, name+":"); ok {
				n, err := strconv.Atoi(value)
				if err != nil {
					t.Fatal(err)
				}
				return n
			}
		}
		t.Fatalf("missing %s in %s", name, info)
		return 0
	}
	deadline := time.Now().Add(10 * time.Second)
	for stat("retained_reply_bytes") < 160<<20 {
		if time.Now().After(deadline) {
			t.Fatal("did not establish aggregate slow-reader pressure")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := call(t, c, reader, "LRANGE", "list", "0", "-1"); got != "-ERR temporary command allocation budget exhausted" {
		t.Fatal(got)
	}
	if stat("command_allocation_refusals") == 0 {
		t.Fatal("reservation refusal was not recorded")
	}
	if got := call(t, c, reader, "LLEN", "list"); got != ":32" {
		t.Fatal(got)
	}
	if got := call(t, c, reader, "SET", "after", "accepted"); got != "+OK" {
		t.Fatal(got)
	}
	for _, conn := range slow {
		conn.Close()
	}
	deadline = time.Now().Add(10 * time.Second)
	for stat("retained_reply_bytes") > 1<<20 {
		if time.Now().After(deadline) {
			t.Fatal("retained replies did not release after disconnect")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := call(t, c, reader, "LRANGE", "list", "0", "-1"); got != "*32" {
		t.Fatal(got)
	}
	for i := 0; i < 32; i++ {
		if got := reply(t, reader); got != member {
			t.Fatalf("member %d corrupted", i)
		}
	}
	if got := call(t, c, reader, "GET", "after"); got != "accepted" {
		t.Fatal(got)
	}
	s.stop(t)
}
