package core

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/brandopakel/keel/internal/constant"
)

func TestSETAndGET(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.Equal(t, constant.RespNil, rawReplyOn(t, e, "GET", "nokey"))
	assert.Equal(t, "OK", runOn(t, e, "SET", "k", "v"))
	assert.Equal(t, "v", runOn(t, e, "GET", "k"))
	assert.Equal(t, "OK", runOn(t, e, "SET", "k", "w"), "a second SET replaces")
	assert.Equal(t, "w", runOn(t, e, "GET", "k"))
	assert.Equal(t, "OK", runOn(t, e, "SET", "empty", ""))
	assert.Equal(t, "$0\r\n\r\n", string(rawReplyOn(t, e, "GET", "empty")), "an empty value is a value, not nil")

	assert.Contains(t, runOn(t, e, "SET", "k"), "wrong number of arguments")
	assert.Contains(t, runOn(t, e, "SET", "k", "v", "EX"), "syntax error", "an option without its value")
	assert.Contains(t, runOn(t, e, "SET", "k", "v", "EX", "x"), "not an integer")
	assert.Contains(t, runOn(t, e, "GET"), "wrong number of arguments")
	assert.Contains(t, runOn(t, e, "GET", "a", "b"), "wrong number of arguments")
}

func TestSETReplacesAnExpiry(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "SET", "k", "v", "EX", "100")
	assert.EqualValues(t, 100, runOn(t, e, "TTL", "k"))
	runOn(t, e, "SET", "k", "v")
	assert.EqualValues(t, -1, runOn(t, e, "TTL", "k"), "a plain SET drops the TTL, as Redis without KEEPTTL does")
}

func TestINCR(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.EqualValues(t, 1, runOn(t, e, "INCR", "n"), "a missing key counts from zero")
	assert.EqualValues(t, 2, runOn(t, e, "INCR", "n"))
	assert.Equal(t, "2", runOn(t, e, "GET", "n"), "the value is a string that reads as the number")

	runOn(t, e, "SET", "neg", "-5")
	assert.EqualValues(t, -4, runOn(t, e, "INCR", "neg"))

	runOn(t, e, "SET", "s", "hello")
	assert.Contains(t, runOn(t, e, "INCR", "s"), "not an integer or out of range")
	assert.Equal(t, "hello", runOn(t, e, "GET", "s"), "a refused INCR leaves the value alone")

	runOn(t, e, "SET", "padded", "007")
	assert.Contains(t, runOn(t, e, "INCR", "padded"), "not an integer", "leading zeros are not a canonical integer")

	runOn(t, e, "SET", "big", "9223372036854775807")
	assert.Contains(t, runOn(t, e, "INCR", "big"), "would overflow")
	assert.Equal(t, "9223372036854775807", runOn(t, e, "GET", "big"))

	assert.Contains(t, runOn(t, e, "INCR"), "wrong number of arguments")
}

func TestINCRKeepsTheTTL(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "SET", "n", "1", "EX", "100")
	assert.EqualValues(t, 2, runOn(t, e, "INCR", "n"))
	assert.InDelta(t, 100, runOn(t, e, "TTL", "n"), 1, "incrementing is not a rewrite; the TTL stays")
}

func TestTTLAndPTTL(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.EqualValues(t, -2, runOn(t, e, "TTL", "nokey"))
	assert.EqualValues(t, -2, runOn(t, e, "PTTL", "nokey"))

	runOn(t, e, "SET", "forever", "v")
	assert.EqualValues(t, -1, runOn(t, e, "TTL", "forever"))
	assert.EqualValues(t, -1, runOn(t, e, "PTTL", "forever"))

	runOn(t, e, "SET", "k", "v", "PX", "2500")
	pttl := runOn(t, e, "PTTL", "k").(int64)
	assert.True(t, pttl > 2000 && pttl <= 2500, "PTTL %d should be in milliseconds", pttl)
	// 2500ms rounds to 3 and 2499ms to 2, so either answer shows the rounding
	// without pinning the test to the millisecond between the two commands.
	assert.Contains(t, []interface{}{int64(2), int64(3)}, runOn(t, e, "TTL", "k"),
		"TTL rounds to the nearest second, as Redis does")

	assert.Contains(t, runOn(t, e, "TTL"), "wrong number of arguments")
	assert.Contains(t, runOn(t, e, "PTTL", "a", "b"), "wrong number of arguments")
}

func TestEXPIRE(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.EqualValues(t, 0, runOn(t, e, "EXPIRE", "nokey", "10"), "nothing to expire")

	runOn(t, e, "SET", "k", "v")
	assert.EqualValues(t, 1, runOn(t, e, "EXPIRE", "k", "10"))
	assert.InDelta(t, 10, runOn(t, e, "TTL", "k"), 1)

	assert.EqualValues(t, 1, runOn(t, e, "EXPIRE", "k", "0"), "a time already passed is a delete")
	assert.EqualValues(t, 0, runOn(t, e, "EXISTS", "k"))
	runOn(t, e, "SET", "k", "v")
	assert.EqualValues(t, 1, runOn(t, e, "EXPIRE", "k", "-5"))
	assert.EqualValues(t, 0, runOn(t, e, "EXISTS", "k"))

	runOn(t, e, "SET", "k", "v")
	assert.Contains(t, runOn(t, e, "EXPIRE", "k", "soon"), "not an integer")
	assert.Contains(t, runOn(t, e, "EXPIRE", "k", "9223372036854775807"), "invalid expire time")
	assert.EqualValues(t, -1, runOn(t, e, "TTL", "k"), "a refused EXPIRE sets nothing")

	// SET is bounded the same way: a duration that would carry the instant
	// past what the clock can hold is refused rather than stored and wrapped.
	assert.Contains(t, runOn(t, e, "SET", "far", "v", "EX", "9223372036854775"), "invalid expire time")
	assert.Contains(t, runOn(t, e, "SET", "far", "v", "PX", "9223372036854775807"), "invalid expire time")
	assert.EqualValues(t, 0, runOn(t, e, "EXISTS", "far"), "and nothing was written")
	assert.Contains(t, runOn(t, e, "EXPIRE", "k"), "wrong number of arguments")
}

func TestExpiredKeyReadsAsAbsent(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "SET", "k", "v", "PX", "1")
	time.Sleep(5 * time.Millisecond)
	assert.Equal(t, constant.RespNil, rawReplyOn(t, e, "GET", "k"))
	assert.EqualValues(t, -2, runOn(t, e, "TTL", "k"))
	assert.EqualValues(t, 0, runOn(t, e, "EXISTS", "k"))
}

func TestDELNeedsAKey(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.Contains(t, runOn(t, e, "DEL"), "wrong number of arguments")
	runOn(t, e, "SET", "a", "1")
	runOn(t, e, "SADD", "s", "m")
	assert.EqualValues(t, 2, runOn(t, e, "DEL", "a", "s", "nokey"), "every type, counted once each")
}
