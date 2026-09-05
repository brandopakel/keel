package core

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPING(t *testing.T) {
	ResetStores()
	assert.Equal(t, "+PONG\r\n", string(rawReply(t, "PING")))
	assert.Equal(t, "$5\r\nhello\r\n", string(rawReply(t, "PING", "hello")), "an argument is echoed as a bulk string")
	assert.Contains(t, run(t, "PING", "a", "b"), "wrong number of arguments")
}

// TestUnknownCommandIsAnError. The error comes back from EvalAndResponse rather
// than as a reply, because the one other caller is the log replay, which has to
// stop on a command it cannot run rather than skip it.
func TestUnknownCommandIsAnError(t *testing.T) {
	ResetStores()
	var w replyWriter
	err := EvalAndResponse(&Command{Cmd: "NOSUCH", Args: []string{"a"}}, &w)
	assert.EqualError(t, err, "ERR unknown command 'NOSUCH'")
	assert.Empty(t, w.b, "nothing is written for it here; the caller replies")
}

// TestAnErrorQuotingClientInputStaysOneFrame: a command name or argument can
// carry CRLF inside a bulk string, and an error that quotes it would otherwise
// end at the first one, leaving the rest to be read as the next reply.
func TestAnErrorQuotingClientInputStaysOneFrame(t *testing.T) {
	ResetStores()
	var w replyWriter
	err := EvalAndResponse(&Command{Cmd: "NO\r\nSUCH"}, &w)
	reply := Encode(err, false)
	assert.Equal(t, "-ERR unknown command 'NO  SUCH'\r\n", string(reply))
	assert.Equal(t, 1, bytes.Count(reply, []byte("\r\n")), "one frame")

	// A handler that quotes an argument goes through the same encoder.
	raw := rawReply(t, "GEOSEARCH", "g", "FROMLONLAT", "1", "1", "BYRADIUS", "1", "km\r\n:1")
	assert.Equal(t, byte('-'), raw[0])
	assert.Equal(t, 1, bytes.Count(raw, []byte("\r\n")), "one frame: %q", raw)
}

func TestEveryRegisteredCommandIsTypeCheckedOrDeliberatelyNot(t *testing.T) {
	// Commands that answer about a name whatever type holds it are absent from
	// the type table on purpose; everything else in the dispatch table has to
	// be in it, or a name held by another type would slip through.
	exempt := map[string]bool{
		"PING": true, "DEL": true, "EXISTS": true, "TYPE": true, "KEYS": true, "MGET": true,
		"FLUSHDB": true, "DBSIZE": true, "MEMORY": true, "INFO": true, "BGREWRITEAOF": true,
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
	ResetStores()
	run(t, "SADD", "s", "a")
	assert.Equal(t, "a", run(t, "SRAND", "s"))
	dumped := run(t, "KEEL.DUMP", "s")
	assert.NotEmpty(t, dumped)
	assert.NotContains(t, dumped, "unknown command", "the current name has to work before the alias means anything")
	assert.Equal(t, dumped, run(t, "MEMKV.DUMP", "s"))
}
