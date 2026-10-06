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
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.EqualValues(t, 1, runOn(t, e, "SETNX", "k", "first"))
	assert.EqualValues(t, 0, runOn(t, e, "SETNX", "k", "second"), "an existing key is left alone")
	assert.Equal(t, "first", runOn(t, e, "GET", "k"))
	assert.Contains(t, runOn(t, e, "SETNX", "k"), "wrong number of arguments")
	assert.Contains(t, runOn(t, e, "SETNX", "k", "v", "EX"), "wrong number of arguments", "SETNX takes no options")
}

func TestSETNXIsLoggedAsTheSETItPerforms(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := withAOFOn(t, e, func() {
		runOn(t, e, "SETNX", "k", "v")
		runOn(t, e, "SETNX", "k", "ignored")
	})
	log, err := os.ReadFile(path)
	assert.NoError(t, err)
	assert.NotContains(t, string(log), "SETNX", "a build without SETNX must still replay this log")
	assert.Equal(t, 1, strings.Count(string(log), "\r\nSET\r\n"), "the refused SETNX changed nothing and is not logged")
	restartOn(t, e, path)
	assert.Equal(t, "v", runOn(t, e, "GET", "k"))
}

func TestUNLINKIsDEL(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := withAOFOn(t, e, func() {
		runOn(t, e, "SET", "a", "1")
		runOn(t, e, "SADD", "s", "m")
		assert.EqualValues(t, 2, runOn(t, e, "UNLINK", "a", "s", "missing"), "counts what it removed, of any type")
	})
	assert.Contains(t, runOn(t, e, "UNLINK"), "wrong number of arguments")
	log, err := os.ReadFile(path)
	assert.NoError(t, err)
	assert.NotContains(t, string(log), "UNLINK", "logged as DEL, so a rollback can replay it")
	assert.Contains(t, string(log), "\r\nDEL\r\n")
	restartOn(t, e, path)
	assert.EqualValues(t, 0, runOn(t, e, "EXISTS", "a", "s"))
}

func TestSELECTAcceptsOnlyTheOneDatabase(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.Equal(t, "OK", runOn(t, e, "SELECT", "0"))
	assert.Contains(t, runOn(t, e, "SELECT", "1"), "DB index is out of range",
		"another database is refused, not quietly mapped onto this one")
	assert.Contains(t, runOn(t, e, "SELECT", "x"), "not an integer")
	assert.Contains(t, runOn(t, e, "SELECT"), "wrong number of arguments")
}

func TestECHO(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.Equal(t, "hello world", runOn(t, e, "ECHO", "hello world"))
	assert.Equal(t, "$0\r\n\r\n", string(rawReplyOn(t, e, "ECHO", "")))
	assert.Contains(t, runOn(t, e, "ECHO"), "wrong number of arguments")
	assert.Contains(t, runOn(t, e, "ECHO", "a", "b"), "wrong number of arguments")
}

func TestINFOReportsTheRedisVersionClientsGateOn(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	res, _ := Decode(e.cmdINFO([]string{"server"}))
	out := res.(string)
	assert.Contains(t, out, "redis_version:"+RedisCompatibleVersion+"\r\n")
	assert.Contains(t, out, "redis_mode:standalone\r\n")
	assert.Contains(t, out, "keel_version:", "the server's own version is still there")
}
