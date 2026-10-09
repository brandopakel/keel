package core

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeClient is a connection as the slow log sees one.
type fakeClient struct{ addr, name string }

func (c fakeClient) PeerAddr() string   { return c.addr }
func (c fakeClient) ClientName() string { return c.name }

// slowEngine is an engine whose slow log keeps every command, as Redis's does
// with slowlog-log-slower-than 0.
func slowEngine(t *testing.T) *Engine {
	t.Helper()
	e := newTestEngine(t, Options{})
	e.slowlog.slowerThan = 0
	return e
}

// slowlogGet is SLOWLOG GET's reply, each entry's seven fields.
func slowlogGet(t *testing.T, e *Engine, args ...string) [][]interface{} {
	t.Helper()
	var out [][]interface{}
	for _, entry := range runOn(t, e, "SLOWLOG", append([]string{"GET"}, args...)...).([]interface{}) {
		out = append(out, entry.([]interface{}))
	}
	return out
}

// TestSlowlogEntriesAreRedisEntries: id, time, duration, the arguments as
// logged, the client's address and name, and the real argument count; newest
// first, ids rising, at most slowlog-max-len kept.
func TestSlowlogEntriesAreRedisEntries(t *testing.T) {
	t.Parallel()
	e := slowEngine(t)
	var w replyWriter
	require.NoError(t, e.EvalAndResponse(&Command{Cmd: "SET", Name: "set", Args: []string{"k", "v"},
		Client: fakeClient{"127.0.0.1:50000", "app"}}, &w))
	runOn(t, e, "GET", "k")
	entries := slowlogGet(t, e)
	require.Len(t, entries, 2)
	get, set := entries[0], entries[1]
	assert.Equal(t, int64(1), get[0])
	assert.Equal(t, int64(0), set[0])
	assert.InDelta(t, float64(set[1].(int64)), float64(get[1].(int64)), 2, "Unix seconds")
	assert.GreaterOrEqual(t, set[2].(int64), int64(0), "microseconds")
	assert.Equal(t, []interface{}{"set", "k", "v"}, set[3], "the name as sent")
	assert.Equal(t, []interface{}{"127.0.0.1:50000", "app", int64(3)}, set[4:])
	assert.Equal(t, []interface{}{"", "", int64(2)}, get[4:], "no connection sent it")
	assert.Equal(t, int64(3), runOn(t, e, "SLOWLOG", "LEN"), "SLOWLOG GET itself, too")

	e.slowlog.maxLen = 3
	for i := 0; i < 5; i++ {
		runOn(t, e, "SET", "k", fmt.Sprint(i))
	}
	assert.Equal(t, int64(3), runOn(t, e, "SLOWLOG", "LEN"))
	assert.Equal(t, "OK", runOn(t, e, "SLOWLOG", "RESET"))
	assert.Equal(t, int64(1), runOn(t, e, "SLOWLOG", "LEN"), "RESET itself is logged after it")
}

// TestSlowlogTrimsAsRedisDoes: at most 32 arguments, the last place saying
// how many more there were; each cut at 128 bytes, saying how many more.
func TestSlowlogTrimsAsRedisDoes(t *testing.T) {
	t.Parallel()
	e := slowEngine(t)
	members := make([]string, 40)
	for i := range members {
		members[i] = fmt.Sprint(i)
	}
	runOn(t, e, "SADD", append([]string{"s"}, members...)...)
	runOn(t, e, "SET", "k", strings.Repeat("v", 200))
	entries := slowlogGet(t, e)
	set, sadd := entries[0][3].([]interface{}), entries[1][3].([]interface{})
	assert.Equal(t, []interface{}{"SET", "k", strings.Repeat("v", 128) + "... (72 more bytes)"}, set)
	require.Len(t, sadd, 32)
	assert.Equal(t, "... (11 more arguments)", sadd[31], "42 arguments, 31 shown")
	assert.Equal(t, int64(42), entries[1][6])
}

// TestSlowlogRedactsSecrets: AUTH's arguments, HELLO's AUTH user and password,
// and a sensitive setting's value in CONFIG SET, as Redis redacts them, and
// also where Redis refuses the command before it redacts them.
func TestSlowlogRedactsSecrets(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ cmd, want []string }{
		{[]string{"AUTH", "secret"}, []string{"AUTH", redacted}},
		{[]string{"AUTH", "user", "secret"}, []string{"AUTH", redacted, redacted}},
		{[]string{"AUTH", "a", "b", "c"}, []string{"AUTH", redacted, redacted, redacted}},
		{[]string{"HELLO", "3", "AUTH", "user", "secret", "SETNAME", "app"}, []string{"HELLO", "3", "AUTH", redacted, redacted, "SETNAME", "app"}},
		{[]string{"HELLO", "4", "auth", "user", "secret"}, []string{"HELLO", "4", "auth", redacted, redacted}},
		{[]string{"CONFIG", "SET", "maxmemory", "1", "RequirePass", "secret"}, []string{"CONFIG", "SET", "maxmemory", "1", "RequirePass", redacted}},
		{[]string{"CONFIG", "SET", "masterauth", "secret", "x"}, []string{"CONFIG", "SET", "masterauth", redacted, "x"}},
		{[]string{"CONFIG", "GET", "requirepass"}, []string{"CONFIG", "GET", "requirepass"}},
	} {
		argv := append([]string(nil), tc.cmd...)
		redactSlowlogArgs(tc.cmd[0], argv)
		assert.Equal(t, tc.want, argv, strings.Join(tc.cmd, " "))
	}
	e := slowEngine(t)
	start := e.StartCommand()
	e.NoteCommandRan(&Command{Cmd: "AUTH", Args: []string{"secret"}}, start)
	assert.Equal(t, []interface{}{"AUTH", redacted}, slowlogGet(t, e)[0][3])
}

// TestSlowlogSkipsExecAndCountsAsRedis: EXEC is not logged, what it ran is;
// INFO stats and the command's commandstats line count what was; RESETSTAT
// clears the counts and leaves the entries; GET's count is read as Redis's.
func TestSlowlogSkipsExecAndCountsAsRedis(t *testing.T) {
	t.Parallel()
	e := slowEngine(t)
	var tx *Transaction
	var w replyWriter
	for _, c := range [][]string{{"MULTI"}, {"SET", "a", "1"}, {"EXEC"}} {
		var err error
		tx, err = e.Transact(tx, &Command{Cmd: c[0], Args: c[1:]}, &w, nil)
		require.NoError(t, err)
	}
	var names []string
	for _, entry := range slowlogGet(t, e, "-1") {
		names = append(names, entry[3].([]interface{})[0].(string))
	}
	assert.Equal(t, []string{"SET", "MULTI"}, names, "no EXEC")
	stats := statsOn(t, e)
	assert.Equal(t, "3", stats["slowlog_commands_count"], "MULTI, SET and SLOWLOG GET; INFO once it has answered")
	assert.Contains(t, cmdstats(t, e)["set"], "slowlog_count")
	assert.Equal(t, "1", cmdstats(t, e)["set"]["slowlog_count"])

	for args, want := range map[string]string{
		"GET -2":    "-ERR count should be greater than or equal to -1\r\n",
		"GET x":     "-ERR count should be greater than or equal to -1\r\n",
		"GET +1":    "-ERR count should be greater than or equal to -1\r\n",
		"GET 1 2":   "-ERR unknown subcommand or wrong number of arguments for 'GET'. Try SLOWLOG HELP.\r\n",
		"LEN x":     "-ERR wrong number of arguments for 'slowlog|len' command\r\n",
		"nosuch":    "-ERR unknown subcommand 'nosuch'. Try SLOWLOG HELP.\r\n",
		"GET 0":     "*0\r\n",
		"RESET now": "-ERR wrong number of arguments for 'slowlog|reset' command\r\n",
	} {
		assert.Equal(t, want, string(rawReplyOn(t, e, "SLOWLOG", strings.Fields(args)...)), args)
	}

	runOn(t, e, "CONFIG", "RESETSTAT")
	assert.Equal(t, "1", statsOn(t, e)["slowlog_commands_count"], "RESETSTAT itself, after it resets, as in Redis")
	assert.NotZero(t, runOn(t, e, "SLOWLOG", "LEN"), "RESETSTAT keeps the entries")
}

// TestSlowlogSettingsAndReplay: CONFIG GET reports Redis's defaults, and the
// log's replay leaves the slow log as it was, as Redis's does.
func TestSlowlogSettingsAndReplay(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	for name, want := range map[string]string{"slowlog-log-slower-than": "10000", "slowlog-max-len": "128",
		"slowlog-entry-max-argc": "32", "slowlog-entry-max-string-len": "128"} {
		assert.Equal(t, [][2]string{{name, want}}, configPairs(t, e, name), name)
	}
	path := filepath.Join(t.TempDir(), "replay.aof")
	require.NoError(t, os.WriteFile(path, encodeStringArray([]string{"SET", "a", "1"}), 0o600))
	replayed := slowEngine(t)
	_, err := replayed.LoadAOF(path)
	require.NoError(t, err)
	assert.Empty(t, replayed.slowlog.entries)
	assert.Zero(t, replayed.slowlog.count)
}
