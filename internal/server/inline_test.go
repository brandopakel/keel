package server

import (
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// raw writes bytes as they are, without RESP's framing.
func (c *pauseConn) raw(wire string) {
	c.t.Helper()
	_, err := c.conn.Write([]byte(wire))
	require.NoError(c.t, err)
}

// closed reports whether the server has closed the connection, once
// everything it sent has been read.
func (c *pauseConn) closed() bool {
	c.t.Helper()
	require.NoError(c.t, c.conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	_, err := c.r.ReadByte()
	var reset *net.OpError
	return errors.Is(err, io.EOF) || errors.As(err, &reset)
}

// TestInlineCommandsAreRead: a request that does not start with '*' is a line
// of words, as Redis reads one: ended by "\r\n" or a bare "\n", split as
// sdssplitargs splits it, and skipped when it has no words. Inline and RESP
// requests mix in one pipeline.
func TestInlineCommandsAreRead(t *testing.T) {
	connect := startPauseServer(t)
	c := connect()

	c.raw("PING\r\n")
	require.Equal(t, "+PONG\r\n", c.line(2*time.Second))
	c.raw("ping\n")
	require.Equal(t, "+PONG\r\n", c.line(2*time.Second), "a newline alone ends the line")
	c.raw("\r\n\n   \r\n\v\f\r\nPING\r\n")
	require.Equal(t, "+PONG\r\n", c.line(2*time.Second), "lines with no words are skipped")
	require.Equal(t, "", c.line(200*time.Millisecond), "and answered with nothing")

	c.raw("SET k \"a b\\x41\\n\"\r\nGET k\r\n")
	require.Equal(t, "+OK\r\n", c.line(2*time.Second))
	require.Equal(t, "$5\r\n", c.line(2*time.Second))
	require.Equal(t, "a bA\n", c.line(2*time.Second))
	require.Equal(t, "\r\n", c.line(2*time.Second))

	c.raw("SET a 1\r\n*2\r\n$3\r\nGET\r\n$1\r\na\r\nget a\n")
	require.Equal(t, "+OK\r\n", c.line(2*time.Second))
	for range 2 {
		require.Equal(t, "$1\r\n", c.line(2*time.Second))
		require.Equal(t, "1\r\n", c.line(2*time.Second))
	}

	c.raw("nosuch arg\r\n")
	require.Equal(t, "-ERR unknown command 'nosuch', with args beginning with: 'arg' \r\n", c.line(2*time.Second))

	// Split across writes, the line waits for its newline.
	c.raw("SET b ")
	require.Equal(t, "", c.line(200*time.Millisecond))
	c.raw("2\r")
	require.Equal(t, "", c.line(200*time.Millisecond))
	c.raw("\nGET b\r\n")
	require.Equal(t, "+OK\r\n", c.line(2*time.Second))
	require.Equal(t, "$1\r\n", c.line(2*time.Second))
	require.Equal(t, "2\r\n", c.line(2*time.Second))
}

// TestInlineCommandsInTransactionsAndPauses: an inline command is a command
// like any other: queued by MULTI, and held by CLIENT PAUSE.
func TestInlineCommandsInTransactionsAndPauses(t *testing.T) {
	connect := startPauseServer(t)
	admin, c := connect(), connect()

	c.raw("MULTI\r\nSET k v\r\n\r\nGET k\r\nEXEC\r\n")
	for _, want := range []string{"+OK\r\n", "+QUEUED\r\n", "+QUEUED\r\n", "*2\r\n", "+OK\r\n", "$1\r\n", "v\r\n"} {
		require.Equal(t, want, c.line(2*time.Second))
	}

	require.Equal(t, "+OK\r\n", admin.do("CLIENT", "PAUSE", "100000", "WRITE"))
	c.raw("SET k w\r\nGET k\r\n")
	require.Equal(t, "", c.line(300*time.Millisecond), "the write waits")
	require.Equal(t, "+OK\r\n", admin.do("CLIENT", "UNPAUSE"))
	require.Equal(t, "+OK\r\n", c.line(2*time.Second))
	require.Equal(t, "$1\r\n", c.line(2*time.Second))
	require.Equal(t, "w\r\n", c.line(2*time.Second))
}

// TestInlineProtocolErrorsCloseTheConnection: unbalanced quotes and a line
// past 64 KiB are answered with Redis's protocol errors, and the connection
// is closed. What came before the bad request in the same read runs and is
// answered first, as Redis runs it.
func TestInlineProtocolErrorsCloseTheConnection(t *testing.T) {
	connect := startPauseServer(t)

	c := connect()
	c.raw("PING\r\nSET k \"x\r\nPING\r\n")
	require.Equal(t, "+PONG\r\n", c.line(2*time.Second))
	require.Equal(t, "-ERR Protocol error: unbalanced quotes in request\r\n", c.line(2*time.Second))
	require.True(t, c.closed())

	c = connect()
	c.raw("set \"\"\"test-key\"\"\" test-value\r\nping\r\n") // tests/unit/protocol.tcl
	require.Equal(t, "-ERR Protocol error: unbalanced quotes in request\r\n", c.line(2*time.Second))
	require.True(t, c.closed())

	c = connect()
	c.raw("SET k " + strings.Repeat("x", 70000))
	require.Equal(t, "-ERR Protocol error: too big inline request\r\n", c.line(2*time.Second))
	require.True(t, c.closed())

	c = connect()
	c.raw("SET k " + strings.Repeat("x", 70000) + "\r\n")
	require.Equal(t, "-ERR Protocol error: too big inline request\r\n", c.line(2*time.Second),
		"refused whether or not its newline has arrived")
	require.True(t, c.closed())

	// A NUL byte hides the newline after it, as strchr stops at it in Redis:
	// the line waits, runs nothing, and is refused at the limit.
	c = connect()
	c.raw("SET k a\x00b\r\nPING\r\n")
	require.Equal(t, "", c.line(300*time.Millisecond))
	c.raw(strings.Repeat("x", 65536))
	require.Equal(t, "-ERR Protocol error: too big inline request\r\n", c.line(2*time.Second))
	require.True(t, c.closed())

	c = connect()
	c.raw("SET k " + strings.Repeat("x", 60000) + "\r\nSTRLEN k\r\n")
	require.Equal(t, "+OK\r\n", c.line(2*time.Second))
	require.Equal(t, ":60000\r\n", c.line(2*time.Second))

	// A malformed RESP request after a good one: the good one is answered too.
	c = connect()
	c.raw("PING\r\n*1\r\n$x\r\n")
	require.Equal(t, "+PONG\r\n", c.line(2*time.Second))
	require.True(t, strings.HasPrefix(c.line(2*time.Second), "-ERR Protocol error"))
	require.True(t, c.closed())
}
