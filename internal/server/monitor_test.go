package server

import (
	"regexp"
	"testing"
	"time"

	"github.com/brandopakel/keel/internal/core"
	"github.com/stretchr/testify/require"
)

// monitorLinePattern is a line MONITOR sends: the time, the database and the
// sender's address, then the arguments.
var monitorLinePattern = regexp.MustCompile(`^\+\d+\.\d{6} \[0 127\.0\.0\.1:\d+\] (.*)\r\n$`)

// feedLine reads one line a monitor is sent and returns its arguments.
func feedLine(t *testing.T, m *pauseConn) string {
	t.Helper()
	line := m.line(2 * time.Second)
	match := monitorLinePattern.FindStringSubmatch(line)
	require.NotNil(t, match, "%q", line)
	return match[1]
}

// TestMonitorSendsEachCommandAsItRuns: what other connections run, as sent
// and quoted as Redis quotes it; what EXEC runs before EXEC; nothing
// refused before it runs, and nothing Redis keeps from monitors.
func TestMonitorSendsEachCommandAsItRuns(t *testing.T) {
	connect := startPauseServer(t)
	m, c := connect(), connect()
	require.Equal(t, "+OK\r\n", m.do("MONITOR"))

	require.Equal(t, "+OK\r\n", c.do("set", "foo", "bar\n\"x\"\x01"))
	require.Equal(t, `"set" "foo" "bar\n\"x\"\x01"`, feedLine(t, m))
	c.send("GET", "foo")
	require.Equal(t, "bar\n\"x\"\x01", c.bulk())
	require.Equal(t, `"GET" "foo"`, feedLine(t, m))

	// A transaction: MULTI, what EXEC runs, then EXEC; nothing for a command
	// refused while queueing.
	require.Equal(t, "+OK\r\n", c.do("MULTI"))
	require.Equal(t, "+QUEUED\r\n", c.do("SET", "foo", "baz"))
	c.send("EXEC")
	require.Equal(t, "*1\r\n", c.line(2*time.Second))
	c.line(2 * time.Second)
	require.Equal(t, `"MULTI"`, feedLine(t, m))
	require.Equal(t, `"SET" "foo" "baz"`, feedLine(t, m))
	require.Equal(t, `"EXEC"`, feedLine(t, m))
	c.do("MULTI")
	c.do("syntax", "error")
	c.do("EXEC")
	require.Equal(t, `"MULTI"`, feedLine(t, m))
	require.Equal(t, `"EXEC"`, feedLine(t, m))

	// Passwords are redacted; ADMIN commands are not sent; CLIENT's
	// subcommands are judged by their own flags.
	c.do("AUTH", "not-real")
	require.Equal(t, `"AUTH" "(redacted)"`, feedLine(t, m))
	c.send("HELLO", "3", "AUTH", "default", "pw", "SETNAME", "n")
	c.line(2 * time.Second)
	require.Equal(t, `"HELLO" "3" "AUTH" "(redacted)" "(redacted)" "SETNAME" "n"`, feedLine(t, m))
	c.send("HELLO", "2")
	c.line(2 * time.Second)
	require.Equal(t, `"HELLO" "2"`, feedLine(t, m))
	c.send("CONFIG", "GET", "maxmemory")
	c.line(2 * time.Second)
	c.line(2 * time.Second)
	c.line(2 * time.Second)
	c.line(2 * time.Second)
	c.line(2 * time.Second)
	c.do("CLIENT", "ID")
	require.Equal(t, `"CLIENT" "ID"`, feedLine(t, m), "CONFIG GET, an admin command, was not sent")
	c.do("nosuch")
	c.do("PING")
	require.Equal(t, `"PING"`, feedLine(t, m), "an unknown command was not sent")
}

// TestMonitorItselfAndReset: MONITOR again is answered with nothing, inside
// EXEC it is refused, the monitor's line in CLIENT LIST is flagged O, and
// RESET ends it.
func TestMonitorItselfAndReset(t *testing.T) {
	connect := startPauseServer(t)
	m, c := connect(), connect()
	require.Equal(t, "+OK\r\n", m.do("MONITOR"))
	m.send("MONITOR")
	require.Equal(t, "", m.line(200*time.Millisecond), "MONITOR again answers nothing")
	flags := ""
	for _, line := range c.clientLines("LIST") {
		if field(line, "cmd") == "monitor" {
			flags = field(line, "flags")
		}
	}
	require.Equal(t, "O", flags)
	c.do("PING")
	require.Equal(t, `"PING"`, feedLine(t, m), "CLIENT LIST, an admin command, was not sent")

	require.Equal(t, "+OK\r\n", c.do("MULTI"))
	require.Equal(t, "+QUEUED\r\n", c.do("MONITOR"))
	c.send("EXEC")
	require.Equal(t, "*1\r\n", c.line(2*time.Second))
	require.Equal(t, "-ERR MONITOR isn't allowed for DENY BLOCKING client\r\n", c.line(2*time.Second))
	require.Equal(t, `"MULTI"`, feedLine(t, m))
	require.Equal(t, `"EXEC"`, feedLine(t, m))

	require.Equal(t, "+RESET\r\n", m.do("RESET"))
	c.do("PING")
	require.Equal(t, "", m.line(200*time.Millisecond), "no longer monitoring")
}

// TestResetRestoresTheConnection: RESET answers RESET, discards a
// transaction, speaks RESP2, clears the name, turns CLIENT REPLY and the
// modes off, and keeps the library's name.
func TestResetRestoresTheConnection(t *testing.T) {
	connect := startPauseServer(t)
	c := connect()
	c.do("CLIENT", "SETNAME", "me")
	c.do("CLIENT", "SETINFO", "LIB-NAME", "lib")
	c.do("CLIENT", "NO-TOUCH", "on")
	c.send("HELLO", "3")
	for c.line(300*time.Millisecond) != "" {
	}
	require.Equal(t, "+OK\r\n", c.do("MULTI"))
	require.Equal(t, "+RESET\r\n", c.do("RESET"), "not queued")
	require.Equal(t, "-ERR EXEC without MULTI\r\n", c.do("EXEC"))
	require.Equal(t, "$-1\r\n", c.do("CLIENT", "GETNAME"), "RESP2 again, and no name")
	info := c.clientLines("INFO")[0]
	require.Equal(t, "N", field(info, "flags"))
	require.Equal(t, "lib", field(info, "lib-name"))
	require.Equal(t, "2", field(info, "resp"))

	c.send("CLIENT", "REPLY", "OFF")
	c.send("PING")
	require.Equal(t, "+RESET\r\n", c.do("RESET"))
	require.Equal(t, "+PONG\r\n", c.do("PING"))
}

// TestResetFollowsALivePassword: RESET logs the connection out when the
// server has a password, set by CONFIG SET requirepass after it connected,
// and logs it in once the password is taken away, as Redis's
// clearClientConnectionState hands it back to the default user as that user
// is now (networking.c).
func TestResetFollowsALivePassword(t *testing.T) {
	e := newTestEngine(t, core.Options{})
	pass := newServerPassword("")
	e.SetServerInfo(&core.ServerInfo{SetRequirePass: pass.set})
	c := &client{fd: -1, serverPass: pass, authenticated: true, engine: e}
	require.Equal(t, "+OK\r\n", runOnce(t, c, sent("CONFIG", "SET", "requirepass", "secret")))
	require.Equal(t, "$-1\r\n", runOnce(t, c, sent("GET", "k")), "made while no password was needed")
	require.Equal(t, "+RESET\r\n", runOnce(t, c, sent("RESET")))
	require.Equal(t, "-NOAUTH Authentication required.\r\n", runOnce(t, c, sent("GET", "k")), "logged out by RESET")
	require.Equal(t, "+OK\r\n", runOnce(t, c, sent("AUTH", "secret")))

	stranger := &client{fd: -1, serverPass: pass, engine: e}
	require.Equal(t, "-NOAUTH Authentication required.\r\n", runOnce(t, stranger, sent("GET", "k")))
	require.Equal(t, "+OK\r\n", runOnce(t, c, sent("CONFIG", "SET", "requirepass", "")))
	require.Equal(t, "+RESET\r\n", runOnce(t, stranger, sent("RESET")))
	require.Equal(t, "$-1\r\n", runOnce(t, stranger, sent("GET", "k")), "no password, so RESET logs it in")
	require.Equal(t, "+OK\r\n", runOnce(t, c, sent("CONFIG", "SET", "requirepass", "again")))
	require.Equal(t, "$-1\r\n", runOnce(t, stranger, sent("GET", "k")), "logged in stays logged in")
}

func TestAppendReprQuotesAsRedis(t *testing.T) {
	require.Equal(t, `"a\\b\"c\n\r\t\a\b\x00\xff~ "`, string(appendRepr(nil, "a\\b\"c\n\r\t\a\b\x00\xff~ ")))
	line := monitorLine(nil, time.Unix(12, 3000), "1.2.3.4:5", &core.Command{Cmd: "AUTH", Name: "auth", Args: []string{"u", "p"}})
	require.Equal(t, "+12.000003 [0 1.2.3.4:5] \"auth\" \"(redacted)\" \"(redacted)\"\r\n", string(line))
}

// TestAMonitorThatDoesNotReadIsClosed: lines past the output limit kill it.
func TestAMonitorThatDoesNotReadIsClosed(t *testing.T) {
	t.Cleanup(func() { monitors, fedMonitors, killedClients = nil, nil, nil })
	total, input, reply := retainedClientBytes, retainedInputBytes, retainedReplyBytes
	t.Cleanup(func() { retainedClientBytes, retainedInputBytes, retainedReplyBytes = total, input, reply })
	r, _ := socketPair(t)
	m := &client{fd: r, engine: newTestEngine(t, core.Options{})}
	clients[r] = m
	t.Cleanup(func() { delete(clients, r) })
	m.conn.monitor = true
	monitors = []*client{m}
	m.out = make([]byte, maxOutputBuffer)
	feedMonitors(&client{addr: "1.2.3.4:5"}, &core.Command{Cmd: "PING"})
	var arena replyArena
	writable := deliverMonitorFeeds(nil, &arena, nil)
	require.Empty(t, writable)
	require.True(t, m.conn.killed)
	require.Equal(t, []*client{m}, killedClients)
}
