package core

import (
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

func TestEveryRegisteredCommandIsTypeCheckedOrDeliberatelyNot(t *testing.T) {
	// Commands that answer about a name whatever type holds it are absent from
	// the type table on purpose; everything else in the dispatch table has to
	// be in it, or a name held by another type would slip through.
	exempt := map[string]bool{
		"PING": true, "DEL": true, "EXISTS": true, "TYPE": true, "KEYS": true, "MGET": true,
		"FLUSHDB": true, "DBSIZE": true, "MEMORY": true, "INFO": true, "BGREWRITEAOF": true,
		"KEEL.DUMP": true, "KEEL.RESTORE": true, "MEMKV.DUMP": true, "MEMKV.RESTORE": true,
		"LCS": true, "MORRIS.INFO": true,
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
}

func TestOldNamesStillAnswer(t *testing.T) {
	ResetStores()
	run(t, "SADD", "s", "a")
	assert.Equal(t, "a", run(t, "SRAND", "s"))
	assert.Equal(t, run(t, "KEEL.DUMP", "s"), run(t, "MEMKV.DUMP", "s"))
}
