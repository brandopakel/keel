package main

import (
	"bufio"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Client libraries send HELLO, CLIENT and QUIT on their own: redis-py 8 and
// node-redis 6 open every connection with HELLO 3, ioredis 6 sends HELLO 3 with
// the password in it, all of them send CLIENT SETINFO, and quit() sends QUIT.
// These tests are the exchanges those libraries depend on.

// helloFields reads a HELLO reply - the RESP2 array form of a map - into a map.
func helloFields(t *testing.T, r *bufio.Reader, header string) map[string]string {
	t.Helper()
	if header != "*14" {
		t.Fatalf("HELLO answered %q, want a 14-element array", header)
	}
	fields := map[string]string{}
	for i := 0; i < 7; i++ {
		key := reply(t, r)
		value := reply(t, r)
		if key == "modules" && value != "*0" {
			t.Fatalf("modules = %q, want an empty array", value)
		}
		fields[key] = value
	}
	return fields
}

func TestHelloNegotiatesRESP2(t *testing.T) {
	s := startTestServer(t)
	c, r := connectTest(t, s)

	fields := helloFields(t, r, call(t, c, r, "HELLO"))
	if fields["proto"] != ":2" || fields["server"] != "keel" || fields["mode"] != "standalone" || fields["role"] != "master" {
		t.Fatalf("HELLO fields: %v", fields)
	}
	if fields["version"] == "" || !strings.HasPrefix(fields["id"], ":") {
		t.Fatalf("HELLO fields: %v", fields)
	}
	helloFields(t, r, call(t, c, r, "HELLO", "2", "SETNAME", "app"))
	if got := call(t, c, r, "CLIENT", "GETNAME"); got != "app" {
		t.Fatalf("CLIENT GETNAME after HELLO SETNAME = %q", got)
	}

	// The answers clients branch on when they fall back to RESP2.
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"HELLO", "3"}, "-NOPROTO unsupported protocol version"},
		{[]string{"HELLO", "1"}, "-NOPROTO unsupported protocol version"},
		{[]string{"HELLO", "x"}, "-ERR Protocol version is not an integer or out of range"},
		{[]string{"HELLO", "2", "BOGUS"}, "-ERR Syntax error in HELLO option 'BOGUS'"},
		{[]string{"HELLO", "2", "SETNAME", "has space"}, "-ERR Client names cannot contain spaces, newlines or special characters."},
	} {
		if got := call(t, c, r, tc.args...); got != tc.want {
			t.Errorf("%v = %q, want %q", tc.args, got, tc.want)
		}
	}
	if got := call(t, c, r, "PING"); got != "+PONG" {
		t.Fatalf("connection unusable after refused HELLOs: %q", got)
	}
}

func TestHelloBeforeAuthentication(t *testing.T) {
	s := startTestServer(t, "-requirepass-env", "KEEL_TEST_PASSWORD")
	c, r := connectTest(t, s)

	// ioredis 6 sends exactly this and falls back to RESP2 and AUTH only if
	// the answer is NOPROTO. The version is refused before the password is
	// looked at, so the connection is still not logged in afterwards.
	if got := call(t, c, r, "HELLO", "3", "AUTH", "default", "integration-secret"); got != "-NOPROTO unsupported protocol version" {
		t.Fatalf("HELLO 3 AUTH = %q", got)
	}
	if got := call(t, c, r, "GET", "k"); got != "-NOAUTH Authentication required" {
		t.Fatalf("GET after refused HELLO = %q", got)
	}
	if got := call(t, c, r, "HELLO", "2"); !strings.HasPrefix(got, "-NOAUTH HELLO must be called with the client already authenticated") {
		t.Fatalf("HELLO without credentials = %q", got)
	}
	for _, args := range [][]string{
		{"HELLO", "2", "AUTH", "default", "wrong"},
		{"HELLO", "2", "AUTH", "someone", "integration-secret"},
	} {
		if got := call(t, c, r, args...); got != "-WRONGPASS invalid username-password pair" {
			t.Fatalf("%v = %q", args, got)
		}
	}
	if got := call(t, c, r, "CLIENT", "ID"); got != "-NOAUTH Authentication required" {
		t.Fatalf("CLIENT before AUTH = %q", got)
	}
	helloFields(t, r, call(t, c, r, "HELLO", "2", "AUTH", "default", "integration-secret"))
	if got := call(t, c, r, "SET", "k", "v"); got != "+OK" {
		t.Fatalf("SET after HELLO AUTH = %q", got)
	}
}

func TestClientCommands(t *testing.T) {
	s := startTestServer(t)
	c, r := connectTest(t, s)
	other, otherReader := connectTest(t, s)

	id := call(t, c, r, "CLIENT", "ID")
	n, err := strconv.Atoi(strings.TrimPrefix(id, ":"))
	if err != nil || n <= 0 {
		t.Fatalf("CLIENT ID = %q", id)
	}
	if otherID := call(t, other, otherReader, "CLIENT", "ID"); otherID == id {
		t.Fatalf("two connections share id %s", id)
	}

	if got := call(t, c, r, "CLIENT", "GETNAME"); got != "$-1" {
		t.Fatalf("GETNAME before SETNAME = %q", got)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"CLIENT", "SETNAME", "worker-1"}, "+OK"},
		{[]string{"CLIENT", "GETNAME"}, "worker-1"},
		{[]string{"CLIENT", "SETNAME", "has space"}, "-ERR Client names cannot contain spaces, newlines or special characters."},
		{[]string{"CLIENT", "GETNAME"}, "worker-1"},
		{[]string{"CLIENT", "SETINFO", "LIB-NAME", "redis-py"}, "+OK"},
		{[]string{"CLIENT", "SETINFO", "lib-ver", "8.1.0"}, "+OK"},
		{[]string{"CLIENT", "SETINFO", "COLOUR", "blue"}, "-ERR Unrecognized option 'COLOUR'"},
		{[]string{"CLIENT", "SETINFO", "LIB-NAME", "a b"}, "-ERR LIB-NAME cannot contain spaces, newlines or special characters."},
		{[]string{"CLIENT", "SETNAME"}, "-ERR wrong number of arguments for 'client|setname' command"},
		{[]string{"CLIENT", "KILL", "ID", "1"}, "-ERR unknown subcommand 'KILL'. Try CLIENT HELP."},
		{[]string{"CLIENT"}, "-ERR wrong number of arguments for 'client' command"},
	} {
		if got := call(t, c, r, tc.args...); got != tc.want {
			t.Errorf("%v = %q, want %q", tc.args, got, tc.want)
		}
	}
	info := call(t, c, r, "CLIENT", "INFO")
	for _, field := range []string{"id=" + strings.TrimPrefix(id, ":") + " ", "name=worker-1 ", "lib-name=redis-py ", "lib-ver=8.1.0\n"} {
		if !strings.Contains(info, field) {
			t.Errorf("CLIENT INFO %q lacks %q", info, field)
		}
	}
	if got := call(t, other, otherReader, "CLIENT", "GETNAME"); got != "$-1" {
		t.Fatalf("a name set on one connection showed up on another: %q", got)
	}
}

func TestQuitClosesAfterItsReply(t *testing.T) {
	s := startTestServer(t, "-requirepass-env", "KEEL_TEST_PASSWORD")
	c, r := connectTest(t, s)
	if _, err := io.WriteString(c, request("AUTH", "integration-secret")+request("SET", "before", "1")+
		request("QUIT")+request("SET", "after", "1")); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"+OK", "+OK", "+OK"} {
		if got := reply(t, r); got != want {
			t.Fatalf("reply %q, want %q", got, want)
		}
	}
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if b, err := r.ReadByte(); err == nil {
		t.Fatalf("read %q after QUIT; the connection should be closed", b)
	}

	// Nothing pipelined after QUIT ran, and QUIT needs no login.
	check, checkReader := connectTest(t, s)
	call(t, check, checkReader, "AUTH", "integration-secret")
	if got := call(t, check, checkReader, "EXISTS", "before", "after"); got != ":1" {
		t.Fatalf("EXISTS before after = %q, want :1", got)
	}
	anonymous, anonymousReader := connectTest(t, s)
	if got := call(t, anonymous, anonymousReader, "QUIT"); got != "+OK" {
		t.Fatalf("QUIT before AUTH = %q", got)
	}
	anonymous.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := anonymousReader.ReadByte(); err == nil {
		t.Fatal("connection still open after QUIT before AUTH")
	}
}
