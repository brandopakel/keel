package server

import (
	"errors"
	"io"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/brandopakel/keel/internal/data_structure"
	"github.com/stretchr/testify/require"
)

// redisClientFields are the fields of Redis 8.10.1's CLIENT LIST line, in
// its order (catClientInfoString).
var redisClientFields = []string{"id", "addr", "laddr", "fd", "name", "age", "idle", "flags", "db", "sub", "psub",
	"ssub", "multi", "watch", "qbuf", "qbuf-free", "argv-mem", "multi-mem", "rbs", "rbp", "obl", "oll", "omem",
	"omem-shared", "omem-unshared", "tot-mem", "events", "cmd", "user", "redir", "resp", "lib-name", "lib-ver",
	"io-thread", "tot-net-in", "tot-net-out", "tot-cmds", "read-events", "avg-pipeline-len-sum",
	"avg-pipeline-len-cnt"}

// bulk reads a bulk string reply.
func (c *pauseConn) bulk() string {
	c.t.Helper()
	header := c.line(2 * time.Second)
	require.True(c.t, strings.HasPrefix(header, "$"), header)
	n, err := strconv.Atoi(strings.TrimSpace(header[1:]))
	require.NoError(c.t, err, header)
	body := make([]byte, n+2)
	require.NoError(c.t, c.conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	_, err = io.ReadFull(c.r, body)
	require.NoError(c.t, err)
	return string(body[:n])
}

// clientLines sends a CLIENT subcommand whose reply is client lines and
// returns each line's fields, by name and in order.
func (c *pauseConn) clientLines(args ...string) [][][2]string {
	c.t.Helper()
	c.send(append([]string{"CLIENT"}, args...)...)
	var lines [][][2]string
	for _, line := range strings.Split(strings.TrimSuffix(c.bulk(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var fields [][2]string
		for _, f := range strings.Split(line, " ") {
			k, v, ok := strings.Cut(f, "=")
			require.True(c.t, ok, f)
			fields = append(fields, [2]string{k, v})
		}
		lines = append(lines, fields)
	}
	return lines
}

func names(fields [][2]string) []string {
	out := make([]string, len(fields))
	for i, f := range fields {
		out[i] = f[0]
	}
	return out
}

func field(fields [][2]string, name string) string {
	for _, f := range fields {
		if f[0] == name {
			return f[1]
		}
	}
	return "<none>"
}

// TestClientInfoIsRedissLine: CLIENT INFO writes Redis's fields in Redis's
// order, with the values Redis's test of it expects, and counts what the
// connection sent and ran.
func TestClientInfoIsRedissLine(t *testing.T) {
	connect := startPauseServer(t)
	c := connect()
	require.Equal(t, "+OK\r\n", c.do("CLIENT", "SETNAME", "me"))
	c.send("CLIENT", "ID")
	id := strings.TrimSpace(strings.TrimPrefix(c.line(2*time.Second), ":"))

	lines := c.clientLines("INFO")
	require.Len(t, lines, 1)
	f := lines[0]
	require.Equal(t, redisClientFields, names(f))
	sent := len(encodeCmd("PING")) + len(encodeCmd("CLIENT", "SETNAME", "me")) + len(encodeCmd("CLIENT", "ID")) +
		len(encodeCmd("CLIENT", "INFO"))
	for name, want := range map[string]string{
		"id": id, "name": "me", "flags": "N", "db": "0", "sub": "0", "psub": "0", "ssub": "0", "multi": "-1",
		"watch": "0", "qbuf": "26", "argv-mem": "10", "multi-mem": "0", "obl": "0", "oll": "0", "omem": "0",
		"events": "r", "cmd": "client|info", "user": "default", "redir": "-1", "resp": "2", "io-thread": "0",
		"tot-net-in": strconv.Itoa(sent), "tot-cmds": "3", "avg-pipeline-len-sum": "4", "avg-pipeline-len-cnt": "4",
	} {
		require.Equal(t, want, field(f, name), name)
	}
	require.Equal(t, c.conn.LocalAddr().String(), field(f, "addr"), "the peer's address")
	require.Equal(t, c.conn.RemoteAddr().String(), field(f, "laddr"), "the server's")

	// In a transaction: flag x, and multi counts what is queued.
	require.Equal(t, "+OK\r\n", c.do("MULTI"))
	require.Equal(t, "+QUEUED\r\n", c.do("SET", "k", "v"))
	require.Equal(t, "+OK\r\n", c.do("DISCARD"))
	require.Equal(t, "+OK\r\n", c.do("MULTI"))
	require.Equal(t, "+QUEUED\r\n", c.do("GET", "k"))
	require.Equal(t, "+QUEUED\r\n", c.do("GET", "k"))
	c.send("CLIENT", "INFO")
	require.Equal(t, "+QUEUED\r\n", c.line(2*time.Second))
	c.send("EXEC")
	require.Equal(t, "*3\r\n", c.line(2*time.Second))
	c.line(2 * time.Second)
	c.line(2 * time.Second)
	info := c.bulk()
	require.Contains(t, info, " flags=x ")
	require.Contains(t, info, " multi=3 ")
	require.Contains(t, info, " cmd=exec ")
}

// TestClientListListsEveryConnection: in the order they connected, by type,
// by ID, and Redis's refusals.
func TestClientListListsEveryConnection(t *testing.T) {
	connect := startPauseServer(t)
	a, b, c := connect(), connect(), connect()
	all := a.clientLines("LIST")
	require.Len(t, all, 3)
	var ids []string
	for _, line := range all {
		require.Equal(t, redisClientFields, names(line))
		ids = append(ids, field(line, "id"))
	}
	for i := 1; i < len(ids); i++ {
		x, _ := strconv.Atoi(ids[i-1])
		y, _ := strconv.Atoi(ids[i])
		require.Less(t, x, y, "in the order they connected")
	}
	require.Equal(t, "client|list", field(all[0], "cmd"), "the one asking")
	require.Equal(t, "ping", field(all[1], "cmd"))

	require.Len(t, b.clientLines("LIST", "TYPE", "normal"), 3)
	require.Len(t, b.clientLines("LIST", "TYPE", "replica"), 0)
	require.Len(t, b.clientLines("LIST", "type", "PUBSUB"), 0)
	byID := c.clientLines("LIST", "ID", ids[2], "999999", ids[0], "-1")
	require.Len(t, byID, 2)
	require.Equal(t, ids[2], field(byID[0], "id"), "in the order asked")
	require.Equal(t, ids[0], field(byID[1], "id"))

	for args, want := range map[string]string{
		"CLIENT LIST x":                          "-ERR syntax error\r\n",
		"CLIENT LIST ID":                         "-ERR syntax error\r\n",
		"CLIENT LIST TYPE normal x":              "-ERR syntax error\r\n",
		"CLIENT LIST TYPE x":                     "-ERR Unknown client type 'x'\r\n",
		"CLIENT LIST ID 1 x":                     "-ERR Invalid client ID\r\n",
		"CLIENT LIST ID +1":                      "-ERR Invalid client ID\r\n",
		"CLIENT GETREDIR":                        ":-1\r\n",
		"CLIENT UNBLOCK 1":                       ":0\r\n",
		"CLIENT UNBLOCK 1 error":                 ":0\r\n",
		"CLIENT UNBLOCK x":                       "-ERR value is not an integer or out of range\r\n",
		"CLIENT UNBLOCK x y":                     "-ERR CLIENT UNBLOCK reason should be TIMEOUT or ERROR\r\n",
		"CLIENT UNBLOCK 1 timeout x":             "-ERR unknown subcommand or wrong number of arguments for 'UNBLOCK'. Try CLIENT HELP.\r\n",
		"CLIENT UNBLOCK":                         "-ERR wrong number of arguments for 'client|unblock' command\r\n",
		"CLIENT REPLY":                           "-ERR wrong number of arguments for 'client|reply' command\r\n",
		"CLIENT REPLY x":                         "-ERR syntax error\r\n",
		"CLIENT NO-EVICT x":                      "-ERR syntax error\r\n",
		"CLIENT NO-TOUCH x":                      "-ERR syntax error\r\n",
		"CLIENT TRACKINGINFO x":                  "-ERR wrong number of arguments for 'client|trackinginfo' command\r\n",
		"CLIENT KILL":                            "-ERR wrong number of arguments for 'client|kill' command\r\n",
		"CLIENT KILL id 10 wrong_arg":            "-ERR syntax error\r\n",
		"CLIENT KILL id str":                     "-ERR client-id should be greater than 0\r\n",
		"CLIENT KILL id -1":                      "-ERR client-id should be greater than 0\r\n",
		"CLIENT KILL id 0":                       "-ERR client-id should be greater than 0\r\n",
		"CLIENT KILL type wrong_type":            "-ERR Unknown client type 'wrong_type'\r\n",
		"CLIENT KILL user wrong_user":            "-ERR No such user 'wrong_user'\r\n",
		"CLIENT KILL skipme yes_or_no":           "-ERR syntax error\r\n",
		"CLIENT KILL maxage str":                 "-ERR maxage is not an integer or out of range\r\n",
		"CLIENT KILL maxage 9999999999999999999": "-ERR maxage is not an integer or out of range\r\n",
		"CLIENT KILL maxage -1":                  "-ERR maxage should be greater than 0\r\n",
		"CLIENT KILL 000.123.321.567:0000":       "-ERR No such client\r\n",
		"CLIENT KILL 127.0.0.1:":                 "-ERR No such client\r\n",
		"CLIENT KILL id 999999":                  ":0\r\n",
		"CLIENT KILL user default id 999999":     ":0\r\n",
		"CLIENT KILL type master":                ":0\r\n",
		"CLIENT KILL maxage 99999":               ":0\r\n",
	} {
		require.Equal(t, want, c.do(strings.Fields(args)...), args)
	}
	c.send("CLIENT", "TRACKINGINFO")
	for _, want := range []string{"*6\r\n", "$5\r\n", "flags\r\n", "*1\r\n", "$3\r\n", "off\r\n", "$8\r\n", "redirect\r\n",
		":-1\r\n", "$8\r\n", "prefixes\r\n", "*0\r\n"} {
		require.Equal(t, want, c.line(2*time.Second))
	}
}

// TestClientKillClosesConnections: by address, Redis's old form; by ID;
// every other connection, sparing the one asking; and the one asking too,
// once its reply has gone.
func TestClientKillClosesConnections(t *testing.T) {
	connect := startPauseServer(t)
	admin, victim := connect(), connect()
	victimLine := victim.clientLines("INFO")[0]

	require.Equal(t, "+OK\r\n", admin.do("CLIENT", "KILL", field(victimLine, "addr")))
	requireClosed(t, victim)

	victim = connect()
	victimLine = victim.clientLines("INFO")[0]
	require.Equal(t, ":1\r\n", admin.do("CLIENT", "KILL", "ID", field(victimLine, "id"), "LADDR", field(victimLine, "laddr")))
	requireClosed(t, victim)

	others := []*pauseConn{connect(), connect()}
	require.Equal(t, ":2\r\n", admin.do("CLIENT", "KILL", "SKIPME", "yes"))
	for _, o := range others {
		requireClosed(t, o)
	}
	require.Len(t, admin.clientLines("LIST"), 1, "only the one asking is left")

	other := connect()
	admin.send("CLIENT", "KILL", "SKIPME", "no")
	admin.send("PING")
	require.Equal(t, ":2\r\n", admin.line(2*time.Second))
	requireClosed(t, admin)
	requireClosed(t, other)
}

// requireClosed waits for the server to close c: an end of file, or a reset
// when the server closed it with input it had not read, as the connection
// that sent CLIENT KILL SKIPME no and then PING is.
func requireClosed(t *testing.T, c *pauseConn) {
	t.Helper()
	require.NoError(t, c.conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	_, err := c.r.ReadByte()
	if !errors.Is(err, syscall.ECONNRESET) {
		require.ErrorIs(t, err, io.EOF)
	}
}

// TestClientReplyOffSkipOn: OFF silences every reply until ON, which is
// answered; SKIP silences the next command's; ON clears a SKIP.
func TestClientReplyOffSkipOn(t *testing.T) {
	connect := startPauseServer(t)
	c := connect()
	c.send("CLIENT", "REPLY", "OFF")
	c.send("PING", "pong")
	c.send("PING", "pong2")
	c.send("CLIENT", "REPLY", "ON")
	require.Equal(t, "+OK\r\n", c.line(2*time.Second))
	require.Equal(t, "$5\r\n", c.do("PING", "pong3"))
	require.Equal(t, "pong3\r\n", c.line(2*time.Second))

	c.send("CLIENT", "REPLY", "SKIP")
	c.send("PING", "pong")
	c.send("PING", "pong2")
	require.Equal(t, "$5\r\n", c.line(2*time.Second))
	require.Equal(t, "pong2\r\n", c.line(2*time.Second))

	c.send("CLIENT", "REPLY", "SKIP")
	c.send("CLIENT", "REPLY", "ON")
	require.Equal(t, "+OK\r\n", c.line(2*time.Second))
	require.Equal(t, "+PONG\r\n", c.do("PING"))

	// Pipelined, the silenced replies leave the others in place.
	c.send("CLIENT", "REPLY", "SKIP")
	c.send("SET", "k", "1")
	c.send("INCR", "k")
	require.Equal(t, ":2\r\n", c.line(2*time.Second))
}

// TestClientModesShowInFlags: NO-EVICT and NO-TOUCH, Redis's e and T.
func TestClientModesShowInFlags(t *testing.T) {
	connect := startPauseServer(t)
	c := connect()
	require.Equal(t, "+OK\r\n", c.do("CLIENT", "NO-EVICT", "on"))
	require.Equal(t, "e", field(c.clientLines("INFO")[0], "flags"))
	require.Equal(t, "+OK\r\n", c.do("CLIENT", "NO-TOUCH", "ON"))
	require.Equal(t, "eT", field(c.clientLines("INFO")[0], "flags"))
	require.Equal(t, "+OK\r\n", c.do("CLIENT", "NO-EVICT", "off"))
	require.Equal(t, "+OK\r\n", c.do("CLIENT", "no-touch", "off"))
	require.Equal(t, "N", field(c.clientLines("INFO")[0], "flags"))
}

// TestNoTouchLeavesAccessAlone: under no-touch an access is not recorded, and
// the clock does not move.
func TestNoTouchLeavesAccessAlone(t *testing.T) {
	s := data_structure.NewSpace(data_structure.Limits{})
	access := s.NewAccess()
	s.SetNoTouch(true)
	s.Touch(&access)
	require.Equal(t, uint64(0), access)
	require.Equal(t, uint64(0), s.NewAccess(), "the clock did not move")
	s.SetNoTouch(false)
	s.Touch(&access)
	require.Equal(t, uint64(1), access)
}
