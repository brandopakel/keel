package core

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/brandopakel/keel/internal/constant"
)

func TestSADDCountsNewMembers(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.EqualValues(t, 1, runOn(t, e, "SADD", "s", "adele"))
	assert.EqualValues(t, 2, runOn(t, e, "SADD", "s", "adele", "bob", "chris"))
	assert.EqualValues(t, 3, runOn(t, e, "SCARD", "s"))
	assert.Contains(t, runOn(t, e, "SADD", "s"), "wrong number of arguments")
}

func TestSREMDropsAnEmptiedSetAndCreatesNothing(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.EqualValues(t, 0, runOn(t, e, "SREM", "s", "a"))
	assert.EqualValues(t, 0, runOn(t, e, "EXISTS", "s"), "removing from nothing leaves nothing")

	runOn(t, e, "SADD", "s", "a", "b", "c")
	assert.EqualValues(t, 1, runOn(t, e, "SREM", "s", "a", "d"))
	assert.EqualValues(t, 2, runOn(t, e, "SCARD", "s"))
	assert.EqualValues(t, 2, runOn(t, e, "SREM", "s", "b", "c"))
	assert.EqualValues(t, 0, runOn(t, e, "EXISTS", "s"), "the last member gone, the key goes with it")
	assert.Contains(t, runOn(t, e, "SREM", "s"), "wrong number of arguments")
}

func TestSMEMBERSAndSCARD(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.Equal(t, constant.RespEmptyArray, rawReplyOn(t, e, "SMEMBERS", "s"))
	assert.EqualValues(t, 0, runOn(t, e, "SCARD", "s"))
	runOn(t, e, "SADD", "s", "a", "b", "c")
	assert.ElementsMatch(t, []interface{}{"a", "b", "c"}, runOn(t, e, "SMEMBERS", "s"))
	assert.EqualValues(t, 3, runOn(t, e, "SCARD", "s"))
	assert.Contains(t, runOn(t, e, "SMEMBERS"), "wrong number of arguments")
	assert.Contains(t, runOn(t, e, "SCARD", "s", "extra"), "wrong number of arguments")
}

func TestSISMEMBERAndSMISMEMBERAnswerIntegers(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "SADD", "s", "a", "b")
	assert.EqualValues(t, 1, runOn(t, e, "SISMEMBER", "s", "a"))
	assert.EqualValues(t, 0, runOn(t, e, "SISMEMBER", "s", "z"))
	assert.EqualValues(t, 0, runOn(t, e, "SISMEMBER", "nokey", "a"))

	assert.Equal(t, []interface{}{int64(1), int64(0), int64(1)}, runOn(t, e, "SMISMEMBER", "s", "a", "z", "b"))
	assert.Equal(t, "*2\r\n:1\r\n:0\r\n", string(rawReplyOn(t, e, "SMISMEMBER", "s", "a", "z")),
		"the reply is a RESP array of integers a client can decode")
	assert.Equal(t, []interface{}{int64(0), int64(0)}, runOn(t, e, "SMISMEMBER", "nokey", "a", "b"))
	assert.Contains(t, runOn(t, e, "SMISMEMBER", "s"), "wrong number of arguments")
}

func TestSPOP(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.Equal(t, constant.RespNil, rawReplyOn(t, e, "SPOP", "nokey"))
	assert.Equal(t, constant.RespEmptyArray, rawReplyOn(t, e, "SPOP", "nokey", "2"))

	runOn(t, e, "SADD", "s", "a", "b", "c")
	one := runOn(t, e, "SPOP", "s").(string)
	assert.Contains(t, []string{"a", "b", "c"}, one)
	assert.EqualValues(t, 0, runOn(t, e, "SISMEMBER", "s", one), "popped means gone")

	two := runOn(t, e, "SPOP", "s", "5").([]interface{})
	assert.Len(t, two, 2, "asking for more than there is pops what there is")
	assert.EqualValues(t, 0, runOn(t, e, "EXISTS", "s"), "and an emptied set is removed")

	runOn(t, e, "SADD", "s", "a")
	assert.Equal(t, []interface{}{}, runOn(t, e, "SPOP", "s", "0"))
	assert.EqualValues(t, 1, runOn(t, e, "SCARD", "s"))

	assert.Equal(t, "ERR value is out of range, must be positive", runOn(t, e, "SPOP", "s", "-1"))
	assert.Equal(t, "ERR value is out of range, must be positive", runOn(t, e, "SPOP", "s", "many"),
		"Redis words a count that is not a number the same way")
	assert.Equal(t, "ERR syntax error", runOn(t, e, "SPOP", "s", "1", "2"))
}

func TestSRANDMEMBER(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.Equal(t, constant.RespNil, rawReplyOn(t, e, "SRANDMEMBER", "nokey"))
	assert.Equal(t, constant.RespEmptyArray, rawReplyOn(t, e, "SRANDMEMBER", "nokey", "3"))

	runOn(t, e, "SADD", "s", "a", "b", "c")
	one := runOn(t, e, "SRANDMEMBER", "s").(string)
	assert.Contains(t, []string{"a", "b", "c"}, one)
	assert.EqualValues(t, 3, runOn(t, e, "SCARD", "s"), "reading removes nothing")

	two := runOn(t, e, "SRANDMEMBER", "s", "2").([]interface{})
	assert.Len(t, two, 2)
	assert.NotEqual(t, two[0], two[1], "a positive count gives distinct members")

	all := runOn(t, e, "SRANDMEMBER", "s", "10").([]interface{})
	assert.Len(t, all, 3, "no more than the set holds")

	repeats := runOn(t, e, "SRANDMEMBER", "s", "-10").([]interface{})
	assert.Len(t, repeats, 10, "a negative count gives exactly that many, repeats allowed")
	for _, m := range repeats {
		assert.Contains(t, []interface{}{"a", "b", "c"}, m)
	}

	assert.Equal(t, []interface{}{}, runOn(t, e, "SRANDMEMBER", "s", "0"))
	assert.Contains(t, runOn(t, e, "SRANDMEMBER", "s", "x"), "not an integer")
	// A negative count sizes the reply before it is written, so one no client
	// could consume is refused rather than allocated.
	assert.Contains(t, runOn(t, e, "SRANDMEMBER", "s", "-100000000000"), "out of range")
	assert.Len(t, runOn(t, e, "SRANDMEMBER", "s", "100000000000").([]interface{}), 3, "a positive count is only ever the set")
	assert.Equal(t, "ERR syntax error", runOn(t, e, "SRANDMEMBER", "s", "1", "2"))
	assert.Equal(t, "ERR value is out of range, value must between -9223372036854775807 and 9223372036854775807",
		runOn(t, e, "SRANDMEMBER", "s", "-9223372036854775808"))
}

func TestSRANDIsTheOldNameForSRANDMEMBER(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "SADD", "s", "a")
	assert.Equal(t, "a", runOn(t, e, "SRAND", "s"))
	assert.Equal(t, []interface{}{"a"}, runOn(t, e, "SRAND", "s", "1"))
}
