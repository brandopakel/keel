package core

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/brandopakel/keel/internal/constant"
)

func TestSETAndGET(t *testing.T) {
	ResetStores()
	assert.Equal(t, constant.RespNil, rawReply(t, "GET", "nokey"))
	assert.Equal(t, "OK", run(t, "SET", "k", "v"))
	assert.Equal(t, "v", run(t, "GET", "k"))
	assert.Equal(t, "OK", run(t, "SET", "k", "w"), "a second SET replaces")
	assert.Equal(t, "w", run(t, "GET", "k"))
	assert.Equal(t, "OK", run(t, "SET", "empty", ""))
	assert.Equal(t, "$0\r\n\r\n", string(rawReply(t, "GET", "empty")), "an empty value is a value, not nil")

	assert.Contains(t, run(t, "SET", "k"), "wrong number of arguments")
	assert.Contains(t, run(t, "SET", "k", "v", "EX"), "syntax error", "an option without its value")
	assert.Contains(t, run(t, "SET", "k", "v", "EX", "x"), "not an integer")
	assert.Contains(t, run(t, "GET"), "wrong number of arguments")
	assert.Contains(t, run(t, "GET", "a", "b"), "wrong number of arguments")
}

func TestSETReplacesAnExpiry(t *testing.T) {
	ResetStores()
	run(t, "SET", "k", "v", "EX", "100")
	assert.EqualValues(t, 100, run(t, "TTL", "k"))
	run(t, "SET", "k", "v")
	assert.EqualValues(t, -1, run(t, "TTL", "k"), "a plain SET drops the TTL, as Redis without KEEPTTL does")
}

func TestINCR(t *testing.T) {
	ResetStores()
	assert.EqualValues(t, 1, run(t, "INCR", "n"), "a missing key counts from zero")
	assert.EqualValues(t, 2, run(t, "INCR", "n"))
	assert.Equal(t, "2", run(t, "GET", "n"), "the value is a string that reads as the number")

	run(t, "SET", "neg", "-5")
	assert.EqualValues(t, -4, run(t, "INCR", "neg"))

	run(t, "SET", "s", "hello")
	assert.Contains(t, run(t, "INCR", "s"), "not an integer or out of range")
	assert.Equal(t, "hello", run(t, "GET", "s"), "a refused INCR leaves the value alone")

	run(t, "SET", "padded", "007")
	assert.Contains(t, run(t, "INCR", "padded"), "not an integer", "leading zeros are not a canonical integer")

	run(t, "SET", "big", "9223372036854775807")
	assert.Contains(t, run(t, "INCR", "big"), "would overflow")
	assert.Equal(t, "9223372036854775807", run(t, "GET", "big"))

	assert.Contains(t, run(t, "INCR"), "wrong number of arguments")
}

func TestINCRKeepsTheTTL(t *testing.T) {
	ResetStores()
	run(t, "SET", "n", "1", "EX", "100")
	assert.EqualValues(t, 2, run(t, "INCR", "n"))
	assert.InDelta(t, 100, run(t, "TTL", "n"), 1, "incrementing is not a rewrite; the TTL stays")
}

func TestTTLAndPTTL(t *testing.T) {
	ResetStores()
	assert.EqualValues(t, -2, run(t, "TTL", "nokey"))
	assert.EqualValues(t, -2, run(t, "PTTL", "nokey"))

	run(t, "SET", "forever", "v")
	assert.EqualValues(t, -1, run(t, "TTL", "forever"))
	assert.EqualValues(t, -1, run(t, "PTTL", "forever"))

	run(t, "SET", "k", "v", "PX", "2500")
	pttl := run(t, "PTTL", "k").(int64)
	assert.True(t, pttl > 2000 && pttl <= 2500, "PTTL %d should be in milliseconds", pttl)
	assert.EqualValues(t, 3, run(t, "TTL", "k"), "TTL rounds to the nearest second, as Redis does")

	assert.Contains(t, run(t, "TTL"), "wrong number of arguments")
	assert.Contains(t, run(t, "PTTL", "a", "b"), "wrong number of arguments")
}

func TestEXPIRE(t *testing.T) {
	ResetStores()
	assert.EqualValues(t, 0, run(t, "EXPIRE", "nokey", "10"), "nothing to expire")

	run(t, "SET", "k", "v")
	assert.EqualValues(t, 1, run(t, "EXPIRE", "k", "10"))
	assert.InDelta(t, 10, run(t, "TTL", "k"), 1)

	assert.EqualValues(t, 1, run(t, "EXPIRE", "k", "0"), "a time already passed is a delete")
	assert.EqualValues(t, 0, run(t, "EXISTS", "k"))
	run(t, "SET", "k", "v")
	assert.EqualValues(t, 1, run(t, "EXPIRE", "k", "-5"))
	assert.EqualValues(t, 0, run(t, "EXISTS", "k"))

	run(t, "SET", "k", "v")
	assert.Contains(t, run(t, "EXPIRE", "k", "soon"), "not an integer")
	assert.Contains(t, run(t, "EXPIRE", "k", "9223372036854775807"), "invalid expire time")
	assert.EqualValues(t, -1, run(t, "TTL", "k"), "a refused EXPIRE sets nothing")
	assert.Contains(t, run(t, "EXPIRE", "k"), "wrong number of arguments")
}

func TestExpiredKeyReadsAsAbsent(t *testing.T) {
	ResetStores()
	run(t, "SET", "k", "v", "PX", "1")
	time.Sleep(5 * time.Millisecond)
	assert.Equal(t, constant.RespNil, rawReply(t, "GET", "k"))
	assert.EqualValues(t, -2, run(t, "TTL", "k"))
	assert.EqualValues(t, 0, run(t, "EXISTS", "k"))
}

func TestDELNeedsAKey(t *testing.T) {
	ResetStores()
	assert.Contains(t, run(t, "DEL"), "wrong number of arguments")
	run(t, "SET", "a", "1")
	run(t, "SADD", "s", "m")
	assert.EqualValues(t, 2, run(t, "DEL", "a", "s", "nokey"), "every type, counted once each")
}
