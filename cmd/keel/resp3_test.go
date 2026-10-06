package main

import (
	"bufio"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
)

// RESP3, negotiated by HELLO 3 on one connection and given up by HELLO 2.
//
// redis-py 8 and node-redis 6 send HELLO 3 first on every connection and have
// no fallback, and ioredis 6 sends it with the password. Once a connection has
// switched, every reply is framed in RESP3 and every other connection is left
// as it was.

// rawValue reads one reply and returns its exact bytes, whatever its type:
// the aggregates RESP3 adds are read through to their last element.
func rawValue(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(strings.TrimSuffix(line[1:], "\r\n"))
	out := line
	switch line[0] {
	case '$', '=', '!':
		if n >= 0 {
			p := make([]byte, n+2)
			if _, err := io.ReadFull(r, p); err != nil {
				t.Fatal(err)
			}
			out += string(p)
		}
	case '*', '~', '>':
		for i := 0; i < n; i++ {
			out += rawValue(t, r)
		}
	case '%', '|':
		for i := 0; i < 2*n; i++ {
			out += rawValue(t, r)
		}
	}
	return out
}

func callRaw(t *testing.T, c net.Conn, r *bufio.Reader, parts ...string) string {
	t.Helper()
	if _, err := io.WriteString(c, request(parts...)); err != nil {
		t.Fatal(err)
	}
	return rawValue(t, r)
}

// helloMap is the RESP3 HELLO reply for a connection with the given id, as
// CLIENT ID answered it.
func helloMap(id string) string {
	return "%7\r\n$6\r\nserver\r\n$4\r\nkeel\r\n$7\r\nversion\r\n$5\r\n7.0.0\r\n$5\r\nproto\r\n:3\r\n" +
		"$2\r\nid\r\n" + id + "\r\n$4\r\nmode\r\n$10\r\nstandalone\r\n$4\r\nrole\r\n$6\r\nmaster\r\n" +
		"$7\r\nmodules\r\n*0\r\n"
}

func TestHello3SwitchesTheConnection(t *testing.T) {
	t.Parallel()
	s := startTestServer(t)
	c, r := connectTest(t, s)
	other, otherReader := connectTest(t, s)
	id := call(t, c, r, "CLIENT", "ID")

	if got := callRaw(t, c, r, "HELLO", "3"); got != helloMap(id) {
		t.Fatalf("HELLO 3 = %q", got)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"SET", "k", "v"}, "+OK\r\n"},
		{[]string{"GET", "missing"}, "_\r\n"},
		{[]string{"HSET", "h", "f", "v"}, ":1\r\n"},
		{[]string{"HGETALL", "h"}, "%1\r\n$1\r\nf\r\n$1\r\nv\r\n"},
		{[]string{"SADD", "s", "m"}, ":1\r\n"},
		{[]string{"SMEMBERS", "s"}, "~1\r\n$1\r\nm\r\n"},
		{[]string{"ZADD", "z", "2.5", "m"}, ":1\r\n"},
		{[]string{"ZSCORE", "z", "m"}, ",2.5\r\n"},
		{[]string{"ZRANGE", "z", "0", "-1", "WITHSCORES"}, "*1\r\n*2\r\n$1\r\nm\r\n,2.5\r\n"},
		{[]string{"BF.ADD", "bf", "x"}, "#t\r\n"},
		{[]string{"CLIENT", "GETNAME"}, "_\r\n"},
		// node-redis 6 sends this while connecting over RESP3, and goes on
		// without it on the error Redis 8.10.1 gives too.
		{[]string{"CLIENT", "MAINT_NOTIFICATIONS", "ON"}, "-ERR unknown subcommand 'MAINT_NOTIFICATIONS'. Try CLIENT HELP.\r\n"},
		// HELLO without a version reports the protocol in use.
		{[]string{"HELLO"}, helloMap(id)},
	} {
		if got := callRaw(t, c, r, tc.args...); got != tc.want {
			t.Errorf("%v = %q, want %q", tc.args, got, tc.want)
		}
	}
	for _, args := range [][]string{{"CLIENT", "INFO"}, {"INFO", "server"}} {
		got := callRaw(t, c, r, args...)
		if !strings.HasPrefix(got, "=") || !strings.Contains(got, "\r\ntxt:") {
			t.Errorf("%v = %q, want a verbatim string", args, got)
		}
		if !strings.Contains(got, " resp=3 ") && !strings.Contains(got, "resp_version:3\r\n") {
			t.Errorf("%v = %q, want protocol 3", args, got)
		}
	}
	if got := callRaw(t, other, otherReader, "GET", "missing"); got != "$-1\r\n" {
		t.Fatalf("another connection's reply = %q; the protocol belongs to one connection", got)
	}

	// A HELLO that fails changes nothing, including the protocol.
	for _, args := range [][]string{{"HELLO", "4"}, {"HELLO", "2", "SETNAME", "a b"}, {"HELLO", "2", "BOGUS"}} {
		if got := callRaw(t, c, r, args...); got[0] != '-' {
			t.Fatalf("%v = %q", args, got)
		}
	}
	if got := callRaw(t, c, r, "GET", "missing"); got != "_\r\n" {
		t.Fatalf("GET after a failed HELLO = %q", got)
	}

	// HELLO 2 switches back, and its reply is the first in RESP2.
	if got := call(t, c, r, "HELLO", "2"); got != "*14" {
		t.Fatalf("HELLO 2 = %q", got)
	}
	fields := map[string]string{}
	for i := 0; i < 7; i++ {
		key := reply(t, r)
		fields[key] = reply(t, r)
	}
	if fields["proto"] != ":2" {
		t.Fatalf("HELLO 2 fields: %v", fields)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"GET", "missing"}, "$-1\r\n"},
		{[]string{"HGETALL", "h"}, "*2\r\n$1\r\nf\r\n$1\r\nv\r\n"},
		{[]string{"ZSCORE", "z", "m"}, "$3\r\n2.5\r\n"},
		{[]string{"BF.ADD", "bf", "x"}, ":0\r\n"},
	} {
		if got := callRaw(t, c, r, tc.args...); got != tc.want {
			t.Errorf("after HELLO 2, %v = %q, want %q", tc.args, got, tc.want)
		}
	}
	if got := call(t, c, r, "CLIENT", "INFO"); !strings.Contains(got, " resp=2 ") {
		t.Fatalf("CLIENT INFO after HELLO 2 = %q", got)
	}
}

// TestHello3WithAUTH is the handshake ioredis 6 and redis-py 8 send to a
// server with a password: protocol and credentials in one HELLO.
func TestHello3WithAUTH(t *testing.T) {
	t.Parallel()
	s := startTestServer(t, "-requirepass-env", "KEEL_TEST_PASSWORD")
	c, r := connectTest(t, s)

	if got := callRaw(t, c, r, "HELLO", "3"); !strings.HasPrefix(got, "-NOAUTH HELLO must be called with the client already authenticated") {
		t.Fatalf("HELLO 3 without credentials = %q", got)
	}
	if got := callRaw(t, c, r, "HELLO", "3", "AUTH", "default", "wrong"); got != "-WRONGPASS invalid username-password pair or user is disabled.\r\n" {
		t.Fatalf("HELLO 3 AUTH with the wrong password = %q", got)
	}
	// Neither switched the connection: it is still RESP2, and not logged in.
	if got := callRaw(t, c, r, "GET", "missing"); got != "-NOAUTH Authentication required.\r\n" {
		t.Fatalf("GET after refused HELLOs = %q", got)
	}
	if got := callRaw(t, c, r, "AUTH", "integration-secret"); got != "+OK\r\n" {
		t.Fatal(got)
	}
	if got := callRaw(t, c, r, "GET", "missing"); got != "$-1\r\n" {
		t.Fatalf("GET after refused HELLO 3 = %q, want RESP2", got)
	}

	fresh, freshReader := connectTest(t, s)
	got := callRaw(t, fresh, freshReader, "HELLO", "3", "AUTH", "default", "integration-secret", "SETNAME", "app")
	if !strings.HasPrefix(got, "%7\r\n") || !strings.Contains(got, "$5\r\nproto\r\n:3\r\n") {
		t.Fatalf("HELLO 3 AUTH SETNAME = %q", got)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"GET", "missing"}, "_\r\n"},
		{[]string{"CLIENT", "GETNAME"}, "$3\r\napp\r\n"},
		{[]string{"SET", "k", "v"}, "+OK\r\n"},
	} {
		if got := callRaw(t, fresh, freshReader, tc.args...); got != tc.want {
			t.Errorf("%v = %q, want %q", tc.args, got, tc.want)
		}
	}
}

// TestRESP3Pipeline sends the protocol changes and the commands they apply to
// in one write. A command is framed in the protocol its connection has when it
// runs, not when it was read, so the HELLOs take effect mid-batch.
func TestRESP3Pipeline(t *testing.T) {
	t.Parallel()
	s := startTestServer(t)
	c, r := connectTest(t, s)
	id := call(t, c, r, "CLIENT", "ID")
	var batch strings.Builder
	steps := []struct {
		args []string
		want string
	}{
		{[]string{"HELLO", "3"}, helloMap(id)},
		{[]string{"SET", "k", "v"}, "+OK\r\n"},
		{[]string{"GET", "missing"}, "_\r\n"},
		{[]string{"HGETALL", "missing"}, "%0\r\n"},
		{[]string{"ZADD", "z", "1", "a", "inf", "b"}, ":2\r\n"},
		{[]string{"ZPOPMIN", "z"}, "*2\r\n$1\r\na\r\n,1\r\n"},
		{[]string{"MGET", "k", "missing"}, "*2\r\n$1\r\nv\r\n_\r\n"},
		{[]string{"HELLO", "2"}, ""},
		{[]string{"GET", "missing"}, "$-1\r\n"},
		{[]string{"ZSCORE", "z", "b"}, "$3\r\ninf\r\n"},
		{[]string{"HELLO", "3"}, helloMap(id)},
		{[]string{"ZSCORE", "z", "b"}, ",inf\r\n"},
		{[]string{"LPOP", "missing", "2"}, "_\r\n"},
	}
	for _, step := range steps {
		batch.WriteString(request(step.args...))
	}
	if _, err := io.WriteString(c, batch.String()); err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		got := rawValue(t, r)
		if step.want == "" {
			if !strings.HasPrefix(got, "*14\r\n") || !strings.Contains(got, "$5\r\nproto\r\n:2\r\n") {
				t.Errorf("%v = %q", step.args, got)
			}
			continue
		}
		if got != step.want {
			t.Errorf("%v = %q, want %q", step.args, got, step.want)
		}
	}
}

// TestRESP2WireBytesUnchanged: a connection that never sends HELLO 3 is
// answered exactly as before RESP3 existed here, and so is one that went to
// RESP3 and came back with HELLO 2.
func TestRESP2WireBytesUnchanged(t *testing.T) {
	t.Parallel()
	s := startTestServer(t)
	plain, plainReader := connectTest(t, s)
	returned, returnedReader := connectTest(t, s)
	callRaw(t, returned, returnedReader, "HELLO", "3")
	callRaw(t, returned, returnedReader, "HELLO", "2")
	for _, args := range [][]string{{"SET", "k", "v"}, {"HSET", "h", "f", "v"}, {"SADD", "s", "m"}, {"ZADD", "z", "2.5", "m"}} {
		if got := callRaw(t, plain, plainReader, args...); got != "+OK\r\n" && got != ":1\r\n" {
			t.Fatalf("%v = %q", args, got)
		}
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"GET", "missing"}, "$-1\r\n"},
		{[]string{"MGET", "k", "missing"}, "*2\r\n$1\r\nv\r\n$-1\r\n"},
		{[]string{"HGETALL", "h"}, "*2\r\n$1\r\nf\r\n$1\r\nv\r\n"},
		{[]string{"SMEMBERS", "s"}, "*1\r\n$1\r\nm\r\n"},
		{[]string{"SPOP", "missing", "1"}, "*0\r\n"},
		{[]string{"ZSCORE", "z", "m"}, "$3\r\n2.5\r\n"},
		{[]string{"ZRANGE", "z", "0", "-1", "WITHSCORES"}, "*2\r\n$1\r\nm\r\n$3\r\n2.5\r\n"},
		{[]string{"LPOP", "missing", "2"}, "*-1\r\n"},
		{[]string{"GEOPOS", "missing", "m"}, "*1\r\n*-1\r\n"},
		{[]string{"CLIENT", "GETNAME"}, "$-1\r\n"},
		{[]string{"BF.EXISTS", "missing", "x"}, ":0\r\n"},
	} {
		got, again := callRaw(t, plain, plainReader, tc.args...), callRaw(t, returned, returnedReader, tc.args...)
		if got != tc.want {
			t.Errorf("%v = %q, want %q", tc.args, got, tc.want)
		}
		if again != got {
			t.Errorf("%v after HELLO 3 and HELLO 2 = %q, want %q", tc.args, again, got)
		}
	}
	if got := callRaw(t, plain, plainReader, "INFO", "server"); !strings.HasPrefix(got, "$") || !strings.Contains(got, "resp_version:2\r\n") {
		t.Fatalf("INFO server = %q", got)
	}
}

// helloMap2 is the RESP2 HELLO reply for a connection with the given id: the
// same map as helloMap, flattened.
func helloMap2(id string) string {
	return "*14\r\n$6\r\nserver\r\n$4\r\nkeel\r\n$7\r\nversion\r\n$5\r\n7.0.0\r\n$5\r\nproto\r\n:2\r\n" +
		"$2\r\nid\r\n" + id + "\r\n$4\r\nmode\r\n$10\r\nstandalone\r\n$4\r\nrole\r\n$6\r\nmaster\r\n" +
		"$7\r\nmodules\r\n*0\r\n"
}

// TestRESP3Transactions: EXEC answers with each queued command's reply in
// RESP3 on a RESP3 connection, and a HELLO queued inside the transaction
// switches the protocol in its place. Redis 8.10.1 answers these exchanges
// exactly so, apart from HELLO's own fields: from RESP2, MULTI, HELLO 3,
// GET missing, HGETALL missing, EXEC is [the RESP3 HELLO map, _, %0], and the
// connection stays on RESP3 afterwards.
func TestRESP3Transactions(t *testing.T) {
	t.Parallel()
	s := startTestServer(t)
	c, r := connectTest(t, s)
	id := call(t, c, r, "CLIENT", "ID")

	for _, step := range []struct {
		args []string
		want string
	}{
		{[]string{"SET", "k", "v"}, "+OK\r\n"},
		{[]string{"MULTI"}, "+OK\r\n"},
		{[]string{"HELLO", "3"}, "+QUEUED\r\n"},
		{[]string{"GET", "missing"}, "+QUEUED\r\n"},
		{[]string{"HGETALL", "missing"}, "+QUEUED\r\n"},
		{[]string{"EXEC"}, "*3\r\n" + helloMap(id) + "_\r\n%0\r\n"},
		{[]string{"GET", "missing"}, "_\r\n"},

		// Already on RESP3: every reply in RESP3, until a queued HELLO 2.
		{[]string{"HSET", "h", "f", "v"}, ":1\r\n"},
		{[]string{"ZADD", "z", "2.5", "m"}, ":1\r\n"},
		{[]string{"MULTI"}, "+OK\r\n"},
		{[]string{"HGETALL", "h"}, "+QUEUED\r\n"},
		{[]string{"ZSCORE", "z", "m"}, "+QUEUED\r\n"},
		{[]string{"ZRANGE", "z", "0", "-1", "WITHSCORES"}, "+QUEUED\r\n"},
		{[]string{"CLIENT", "GETNAME"}, "+QUEUED\r\n"},
		{[]string{"HELLO", "2"}, "+QUEUED\r\n"},
		{[]string{"GET", "missing"}, "+QUEUED\r\n"},
		{[]string{"ZSCORE", "z", "m"}, "+QUEUED\r\n"},
		{[]string{"EXEC"}, "*7\r\n%1\r\n$1\r\nf\r\n$1\r\nv\r\n,2.5\r\n*1\r\n*2\r\n$1\r\nm\r\n,2.5\r\n_\r\n" +
			helloMap2(id) + "$-1\r\n$3\r\n2.5\r\n"},
		{[]string{"GET", "missing"}, "$-1\r\n"},

		// A queued HELLO that fails changes nothing, here as outside one.
		{[]string{"MULTI"}, "+OK\r\n"},
		{[]string{"HELLO", "4"}, "+QUEUED\r\n"},
		{[]string{"GET", "missing"}, "+QUEUED\r\n"},
		{[]string{"EXEC"}, "*2\r\n-NOPROTO unsupported protocol version\r\n$-1\r\n"},
	} {
		if got := callRaw(t, c, r, step.args...); got != step.want {
			t.Fatalf("%v = %q, want %q", step.args, got, step.want)
		}
	}
}
