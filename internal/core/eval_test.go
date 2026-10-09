package core

import (
	"bytes"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPING(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.Equal(t, "+PONG\r\n", string(rawReplyOn(t, e, "PING")))
	assert.Equal(t, "$5\r\nhello\r\n", string(rawReplyOn(t, e, "PING", "hello")), "an argument is echoed as a bulk string")
	assert.Contains(t, runOn(t, e, "PING", "a", "b"), "wrong number of arguments")
}

// TestUnknownCommandIsAnError. The error comes back from EvalAndResponse rather
// than as a reply, because the one other caller is the log replay, which has to
// stop on a command it cannot run rather than skip it.
func TestUnknownCommandIsAnError(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	var w replyWriter
	err := e.EvalAndResponse(&Command{Cmd: "NOSUCH", Args: []string{"a"}}, &w)
	assert.EqualError(t, err, "ERR unknown command 'NOSUCH', with args beginning with: 'a' ")
	assert.Empty(t, w.b, "nothing is written for it here; the caller replies")
}

// TestUnknownCommandDiagnosticIsBoundedBeforeFormatting: a name or argument
// can fill the query buffer, and the error echoes no more of either than
// Redis does - 128 bytes of the name, and arguments until 128 bytes of them -
// without copying the rest first.
func TestUnknownCommandDiagnosticIsBoundedBeforeFormatting(t *testing.T) {
	// Not parallel: it reads the process's heap statistics, which a test
	// running beside it would move.
	e := newTestEngine(t, Options{})
	huge := strings.Repeat("NO\r\n", 256<<10)
	cmd := &Command{Cmd: huge, Args: []string{huge, huge, huge}}
	var w replyWriter
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	err := e.EvalAndResponse(cmd, &w)
	reply := Encode(err, false)
	runtime.ReadMemStats(&after)
	assert.Error(t, err)
	assert.Empty(t, w.b, "replay must still receive a fatal unknown-command error")
	echo := strings.Repeat("NO  ", 32)
	assert.Equal(t, "-ERR unknown command '"+echo+"', with args beginning with: '"+echo+"' \r\n", string(reply),
		"the first argument fills the 128 bytes, and none follows it")
	assert.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(64<<10), "formatting and sanitizing an unknown name must stay small")
	assert.Equal(t, 1, bytes.Count(reply, []byte("\r\n")))
}

// TestUnknownCommandIsWordedAsRedis8WordsIt, checked against Redis 8.10.1: the
// name as it was sent, the arguments only when there are some, each cut to
// the room left of 128 bytes, a NUL ending one as it ends a C string.
func TestUnknownCommandIsWordedAsRedis8WordsIt(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		cmd  *Command
		want string
	}{
		{&Command{Cmd: "FOO", Name: "foo"}, "ERR unknown command 'foo'"},
		{&Command{Cmd: "FOO", Name: "FoO", Args: []string{"a", "b"}}, "ERR unknown command 'FoO', with args beginning with: 'a' 'b' "},
		{&Command{Cmd: "FOO", Args: []string{""}}, "ERR unknown command 'FOO', with args beginning with: '' "},
		{&Command{Cmd: "", Args: []string{"a"}}, "ERR unknown command '', with args beginning with: 'a' "},
		{&Command{Cmd: strings.Repeat("X", 129)}, "ERR unknown command '" + strings.Repeat("X", 128) + "'"},
		{&Command{Cmd: "FOO", Args: []string{strings.Repeat("a", 125), "b"}},
			"ERR unknown command 'FOO', with args beginning with: '" + strings.Repeat("a", 125) + "' "},
		{&Command{Cmd: "FOO", Args: []string{strings.Repeat("a", 124), "b", "c"}},
			"ERR unknown command 'FOO', with args beginning with: '" + strings.Repeat("a", 124) + "' 'b' "},
		{&Command{Cmd: "FOO", Args: []string{strings.Repeat("a", 200)}},
			"ERR unknown command 'FOO', with args beginning with: '" + strings.Repeat("a", 128) + "' "},
		{&Command{Cmd: "F", Name: "f\x00oo", Args: []string{"a\x00b", "c"}}, "ERR unknown command 'f', with args beginning with: 'a' 'c' "},
		{&Command{Cmd: "FOO", Args: []string{"a\r\nb"}}, "ERR unknown command 'FOO', with args beginning with: 'a  b' "},
		{&Command{Cmd: "FOO", Args: []string{strings.Repeat("\u00e9", 70)}},
			"ERR unknown command 'FOO', with args beginning with: '" + strings.Repeat("\u00e9", 64) + "' "},
	} {
		assert.EqualError(t, unknownCommand(c.cmd), c.want)
	}
	args := make([]string, 60)
	for i := range args {
		args[i] = "ab"
	}
	assert.EqualError(t, unknownCommand(&Command{Cmd: "FOO", Args: args}),
		"ERR unknown command 'FOO', with args beginning with: "+strings.Repeat("'ab' ", 26), "five bytes each, until 128 are passed")
}

// TestAnErrorQuotingClientInputStaysOneFrame: a command name or argument can
// carry CRLF inside a bulk string, and an error that quotes it would otherwise
// end at the first one, leaving the rest to be read as the next reply.
func TestAnErrorQuotingClientInputStaysOneFrame(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	var w replyWriter
	err := e.EvalAndResponse(&Command{Cmd: "NO\r\nSUCH"}, &w)
	reply := Encode(err, false)
	assert.Equal(t, "-ERR unknown command 'NO  SUCH'\r\n", string(reply))
	assert.Equal(t, 1, bytes.Count(reply, []byte("\r\n")), "one frame")

	// A handler that quotes an argument goes through the same encoder.
	raw := rawReplyOn(t, e, "EXPIRE", "k", "1", "NX\r\n:1")
	assert.Equal(t, "-ERR Unsupported option NX  :1\r\n", string(raw))
	assert.Equal(t, 1, bytes.Count(raw, []byte("\r\n")), "one frame: %q", raw)
}

func TestEveryRegisteredCommandIsTypeCheckedOrDeliberatelyNot(t *testing.T) {
	t.Parallel()
	// Commands that answer about a name whatever type holds it are absent from
	// the type table on purpose; everything else in the dispatch table has to
	// be in it, or a name held by another type would slip through.
	exempt := map[string]bool{
		"KEEL.PROMOTE": true, "KEEL.FENCE": true, // a term, not a key
		"KEEL.REPL.PULL":  true, // protocol cursor, not key arguments
		"KEEL.REPL.PULL2": true,
		"PING":            true, "ECHO": true, "SELECT": true, "UNWATCH": true, // no key at all
		"DEL": true, "UNLINK": true, "EXISTS": true, "TYPE": true, "KEYS": true, "MGET": true,
		"FLUSHDB": true, "DBSIZE": true, "MEMORY": true, "INFO": true, "CONFIG": true, "LATENCY": true, "SLOWLOG": true, "BGREWRITEAOF": true, "SCAN": true,
		"KEEL.DUMP": true, "KEEL.RESTORE": true, "MEMKV.DUMP": true, "MEMKV.RESTORE": true,
		"TTL": true, "PTTL": true, "EXPIRE": true, "PEXPIRE": true, "EXPIREAT": true, "PEXPIREAT": true, "PERSIST": true, "MORRIS.INFO": true,
	}
	for name := range commandTable {
		if exempt[name] {
			continue
		}
		_, checked := commandKeyspace[name]
		assert.True(t, checked, "%s is dispatched but not type-checked", name)
	}
	for name := range commandKeyspace {
		_, dispatched := commandTable[name]
		assert.True(t, dispatched, "%s is type-checked but not dispatched", name)
	}
	for name := range writeCommands {
		_, dispatched := commandTable[name]
		assert.True(t, dispatched, "%s is logged but not dispatched", name)
	}
	// The exemptions have to name real commands too, or one removed from the
	// dispatch table would sit here unnoticed.
	for name := range exempt {
		_, dispatched := commandTable[name]
		assert.True(t, dispatched, "%s is exempted from the type check but no longer dispatched", name)
	}
}

func TestOldNamesStillAnswer(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "SADD", "s", "a")
	assert.Equal(t, "a", runOn(t, e, "SRAND", "s"))
	dumped := runOn(t, e, "KEEL.DUMP", "s")
	assert.NotEmpty(t, dumped)
	assert.NotContains(t, dumped, "unknown command", "the current name has to work before the alias means anything")
	assert.Equal(t, dumped, runOn(t, e, "MEMKV.DUMP", "s"))
}
