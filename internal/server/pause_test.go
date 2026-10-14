package server

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brandopakel/keel/internal/core"
	"github.com/stretchr/testify/require"
)

// pauseConn is one connection to a server under test, which sends a command
// and reads its one-line reply.
type pauseConn struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
}

func (c *pauseConn) send(args ...string) {
	c.t.Helper()
	_, err := c.conn.Write(encodeCmd(args...))
	require.NoError(c.t, err)
}

// line reads one reply line, or "" if none arrives within wait.
func (c *pauseConn) line(wait time.Duration) string {
	c.t.Helper()
	require.NoError(c.t, c.conn.SetReadDeadline(time.Now().Add(wait)))
	line, err := c.r.ReadString('\n')
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return ""
	}
	require.NoError(c.t, err)
	return line
}

// do sends a command and returns the first line of its reply.
func (c *pauseConn) do(args ...string) string {
	c.t.Helper()
	c.send(args...)
	return c.line(2 * time.Second)
}

// startPauseServer runs the event loop on an engine of its own and returns a
// way to connect to it.
func startPauseServer(t *testing.T) func() *pauseConn {
	withFreshShutdownState(t)
	e := newTestEngine(t, core.Options{})
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := probe.Addr().(*net.TCPAddr).Port
	require.NoError(t, probe.Close())
	var wg sync.WaitGroup
	wg.Add(1)
	served := make(chan error, 1)
	go func() { served <- RunAsyncTCPServer(&wg, e, Options{Host: "127.0.0.1", Port: port}) }()
	t.Cleanup(func() {
		requestShutdown()
		wake()
		wg.Wait()
		for fd := range clients {
			delete(clients, fd)
		}
		require.NoError(t, <-served)
	})
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	return func() *pauseConn {
		deadline := time.Now().Add(5 * time.Second)
		for {
			require.True(t, time.Now().Before(deadline), "the loop never answered on %s", addr)
			conn, err := net.DialTimeout("tcp", addr, time.Second)
			if err != nil {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			c := &pauseConn{t: t, conn: conn, r: bufio.NewReader(conn)}
			if c.do("PING") == "+PONG\r\n" {
				t.Cleanup(func() { conn.Close() })
				return c
			}
			conn.Close()
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// blocked is INFO's blocked_clients.
func blocked(t *testing.T, c *pauseConn) string {
	t.Helper()
	c.send("INFO", "clients")
	header := c.line(2 * time.Second)
	require.True(t, strings.HasPrefix(header, "$"), header)
	var count string
	for {
		line := c.line(2 * time.Second)
		if strings.HasPrefix(line, "blocked_clients:") {
			count = strings.TrimSpace(strings.TrimPrefix(line, "blocked_clients:"))
		}
		if line == "\r\n" {
			// The section ends with a blank line, then the bulk's own CRLF.
			c.line(2 * time.Second)
			return count
		}
	}
}

// TestClientPauseHoldsWritesAndEverythingAfterThem: under PAUSE WRITE reads
// run and a write waits, with what its connection sent after it; the waiting
// connection counts in blocked_clients; UNPAUSE runs them.
func TestClientPauseHoldsWritesAndEverythingAfterThem(t *testing.T) {
	connect := startPauseServer(t)
	admin, writer, reader := connect(), connect(), connect()

	require.Equal(t, "+OK\r\n", admin.do("CLIENT", "PAUSE", "100000", "WRITE"))
	require.Equal(t, "$-1\r\n", reader.do("GET", "k"), "a read runs")
	writer.send("SET", "k", "v")
	writer.send("GET", "k")
	require.Equal(t, "", writer.line(300*time.Millisecond), "the write waits")
	require.Equal(t, "1", blocked(t, admin))
	require.Equal(t, "$-1\r\n", reader.do("GET", "k"), "nothing was written")
	require.Equal(t, "-ERR wrong number of arguments for 'set' command\r\n", reader.do("SET", "k"),
		"a command refused before it would run is answered at once")

	require.Equal(t, "+OK\r\n", admin.do("CLIENT", "UNPAUSE"))
	require.Equal(t, "+OK\r\n", writer.line(2*time.Second))
	require.Equal(t, "$1\r\n", writer.line(2*time.Second))
	require.Equal(t, "0", blocked(t, admin))
}

// TestClientPauseAllHoldsEveryCommandUntilItEnds: PAUSE ALL holds every
// command, CLIENT's own included, until the pause ends by itself.
func TestClientPauseAllHoldsEveryCommandUntilItEnds(t *testing.T) {
	connect := startPauseServer(t)
	admin, other := connect(), connect()

	start := time.Now()
	require.Equal(t, "+OK\r\n", admin.do("CLIENT", "PAUSE", "300", "ALL"))
	other.send("PING")
	require.Equal(t, "", other.line(100*time.Millisecond), "a read waits under ALL")
	require.Equal(t, "+OK\r\n", admin.do("CLIENT", "PAUSE", "10", "WRITE"), "and so does CLIENT")
	require.Greater(t, time.Since(start), 250*time.Millisecond)
	require.Equal(t, "+PONG\r\n", other.line(2*time.Second), "the read ran once the pause ended")
}

// TestPausesCombineAsRedisCombinesThem: a new pause keeps ALL while ALL holds,
// and the later end; one that has ended is gone; UNPAUSE ends it.
func TestPausesCombineAsRedisCombinesThem(t *testing.T) {
	t.Cleanup(func() { pause, postponed, pauseEased = clientPause{}, nil, false })
	now := time.Now()
	pauseClients(now.Add(time.Minute), true)
	pauseClients(now.Add(time.Second), false)
	require.Equal(t, clientPause{end: now.Add(time.Minute), all: true}, pause)
	unpauseClients()
	require.Equal(t, clientPause{}, pause)
	require.True(t, pauseEased)

	pauseClients(now.Add(time.Minute), false)
	pauseClients(time.Time{}, false)
	require.Equal(t, clientPause{end: now.Add(time.Minute)}, pause, "PAUSE 0 keeps the end there is")
	pause = clientPause{end: now.Add(-time.Millisecond), all: true}
	pauseEased = false
	updatePause(now)
	require.Equal(t, clientPause{}, pause, "an ended pause is gone")
	require.True(t, pauseEased)
	pauseClients(now.Add(time.Minute), false)
	require.False(t, pause.all, "an ended ALL is not kept")
}

// TestClientPauseErrors are Redis's.
func TestClientPauseErrors(t *testing.T) {
	connect := startPauseServer(t)
	c := connect()
	for args, want := range map[string]string{
		"CLIENT PAUSE 10 NONE":             "-ERR CLIENT PAUSE mode must be WRITE or ALL\r\n",
		"CLIENT PAUSE x":                   "-ERR timeout is not an integer or out of range\r\n",
		"CLIENT PAUSE +10":                 "-ERR timeout is not an integer or out of range\r\n",
		"CLIENT PAUSE -1":                  "-ERR timeout is negative\r\n",
		"CLIENT PAUSE 9223372036854775807": "-ERR timeout is out of range\r\n",
		"CLIENT PAUSE":                     "-ERR wrong number of arguments for 'client|pause' command\r\n",
		"CLIENT pause 1 ALL x":             "-ERR unknown subcommand or wrong number of arguments for 'pause'. Try CLIENT HELP.\r\n",
		"CLIENT UNPAUSE x":                 "-ERR wrong number of arguments for 'client|unpause' command\r\n",
		"CLIENT PAUSE 0":                   "+OK\r\n",
		"CLIENT UNPAUSE":                   "+OK\r\n",
	} {
		require.Equal(t, want, c.do(strings.Fields(args)...), args)
	}
	require.Equal(t, "+PONG\r\n", c.do("PING"), "PAUSE 0 pauses nothing")
}

func TestRedisInt(t *testing.T) {
	for s, want := range map[string]int64{"0": 0, "10": 10, "-5": -5, "9223372036854775807": 1<<63 - 1,
		"-9223372036854775808": -1 << 63} {
		got, ok := redisInt(s)
		require.True(t, ok, s)
		require.Equal(t, want, got, s)
	}
	for _, s := range []string{"", "-", "+1", "01", "-0", "1 ", "9223372036854775808", "1e3", "-9223372036854775809"} {
		_, ok := redisInt(s)
		require.False(t, ok, s)
	}
}

// TestAPauseAnswersNOAUTHAtOnce: a connection that has not logged in to a
// server with a password, set at start or by CONFIG SET requirepass since,
// is refused at once under a pause, as Redis refuses it before it pauses.
func TestAPauseAnswersNOAUTHAtOnce(t *testing.T) {
	t.Cleanup(func() { pause = clientPause{} })
	pause = clientPause{end: time.Now().Add(time.Minute), all: true}
	set := &core.Command{Cmd: "SET", Args: []string{"k", "v"}}
	auth := newServerPassword("")
	c := &client{serverPass: auth, authenticated: true}
	require.True(t, c.waitsForPause(set), "a logged-in connection waits")
	auth.set("secret")
	c = &client{serverPass: auth}
	require.False(t, c.waitsForPause(set), "one that has not logged in is refused at once")
	require.True(t, c.waitsForPause(&core.Command{Cmd: "AUTH", Args: []string{"secret"}}), "AUTH itself waits under ALL")
}
