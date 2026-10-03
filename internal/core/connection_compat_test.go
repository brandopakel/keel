package core

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The commands below exist because client libraries send them unprompted:
// SETNX from cachelib and Flask-Caching's add(), UNLINK from Rails' cache store
// and keyv, SELECT 0 from any connection URL ending in /0, ECHO from health
// checks. Each test is the behaviour a library relies on.

func TestSETNXAnswersOneOrZero(t *testing.T) {
	ResetStores()
	assert.EqualValues(t, 1, run(t, "SETNX", "k", "first"))
	assert.EqualValues(t, 0, run(t, "SETNX", "k", "second"), "an existing key is left alone")
	assert.Equal(t, "first", run(t, "GET", "k"))
	assert.Contains(t, run(t, "SETNX", "k"), "wrong number of arguments")
	assert.Contains(t, run(t, "SETNX", "k", "v", "EX"), "wrong number of arguments", "SETNX takes no options")
}

func TestSETNXIsLoggedAsTheSETItPerforms(t *testing.T) {
	path := withAOF(t, func() {
		run(t, "SETNX", "k", "v")
		run(t, "SETNX", "k", "ignored")
	})
	log, err := os.ReadFile(path)
	assert.NoError(t, err)
	assert.NotContains(t, string(log), "SETNX", "a build without SETNX must still replay this log")
	assert.Equal(t, 1, strings.Count(string(log), "\r\nSET\r\n"), "the refused SETNX changed nothing and is not logged")
	restart(t, path)
	assert.Equal(t, "v", run(t, "GET", "k"))
}

func TestUNLINKIsDEL(t *testing.T) {
	path := withAOF(t, func() {
		run(t, "SET", "a", "1")
		run(t, "SADD", "s", "m")
		assert.EqualValues(t, 2, run(t, "UNLINK", "a", "s", "missing"), "counts what it removed, of any type")
	})
	assert.Contains(t, run(t, "UNLINK"), "wrong number of arguments")
	log, err := os.ReadFile(path)
	assert.NoError(t, err)
	assert.NotContains(t, string(log), "UNLINK", "logged as DEL, so a rollback can replay it")
	assert.Contains(t, string(log), "\r\nDEL\r\n")
	restart(t, path)
	assert.EqualValues(t, 0, run(t, "EXISTS", "a", "s"))
}

func TestSELECTAcceptsOnlyTheOneDatabase(t *testing.T) {
	ResetStores()
	assert.Equal(t, "OK", run(t, "SELECT", "0"))
	assert.Contains(t, run(t, "SELECT", "1"), "DB index is out of range",
		"another database is refused, not quietly mapped onto this one")
	assert.Contains(t, run(t, "SELECT", "x"), "not an integer")
	assert.Contains(t, run(t, "SELECT"), "wrong number of arguments")
}

func TestECHO(t *testing.T) {
	assert.Equal(t, "hello world", run(t, "ECHO", "hello world"))
	assert.Equal(t, "$0\r\n\r\n", string(rawReply(t, "ECHO", "")))
	assert.Contains(t, run(t, "ECHO"), "wrong number of arguments")
	assert.Contains(t, run(t, "ECHO", "a", "b"), "wrong number of arguments")
}

func TestINFOReportsTheRedisVersionClientsGateOn(t *testing.T) {
	res, _ := Decode(cmdINFO([]string{"server"}))
	out := res.(string)
	assert.Contains(t, out, "redis_version:"+RedisCompatibleVersion+"\r\n")
	assert.Contains(t, out, "redis_mode:standalone\r\n")
	assert.Contains(t, out, "keel_version:", "the server's own version is still there")
}
