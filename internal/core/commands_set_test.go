package core

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/brandopakel/keel/internal/constant"
)

func TestSADDCountsNewMembers(t *testing.T) {
	ResetStores()
	assert.EqualValues(t, 1, run(t, "SADD", "s", "adele"))
	assert.EqualValues(t, 2, run(t, "SADD", "s", "adele", "bob", "chris"))
	assert.EqualValues(t, 3, run(t, "SCARD", "s"))
	assert.Contains(t, run(t, "SADD", "s"), "wrong number of arguments")
}

func TestSREMDropsAnEmptiedSetAndCreatesNothing(t *testing.T) {
	ResetStores()
	assert.EqualValues(t, 0, run(t, "SREM", "s", "a"))
	assert.EqualValues(t, 0, run(t, "EXISTS", "s"), "removing from nothing leaves nothing")

	run(t, "SADD", "s", "a", "b", "c")
	assert.EqualValues(t, 1, run(t, "SREM", "s", "a", "d"))
	assert.EqualValues(t, 2, run(t, "SCARD", "s"))
	assert.EqualValues(t, 2, run(t, "SREM", "s", "b", "c"))
	assert.EqualValues(t, 0, run(t, "EXISTS", "s"), "the last member gone, the key goes with it")
	assert.Contains(t, run(t, "SREM", "s"), "wrong number of arguments")
}

func TestSMEMBERSAndSCARD(t *testing.T) {
	ResetStores()
	assert.Equal(t, constant.RespEmptyArray, rawReply(t, "SMEMBERS", "s"))
	assert.EqualValues(t, 0, run(t, "SCARD", "s"))
	run(t, "SADD", "s", "a", "b", "c")
	assert.ElementsMatch(t, []interface{}{"a", "b", "c"}, run(t, "SMEMBERS", "s"))
	assert.EqualValues(t, 3, run(t, "SCARD", "s"))
	assert.Contains(t, run(t, "SMEMBERS"), "wrong number of arguments")
	assert.Contains(t, run(t, "SCARD", "s", "extra"), "wrong number of arguments")
}

func TestSISMEMBERAndSMISMEMBERAnswerIntegers(t *testing.T) {
	ResetStores()
	run(t, "SADD", "s", "a", "b")
	assert.EqualValues(t, 1, run(t, "SISMEMBER", "s", "a"))
	assert.EqualValues(t, 0, run(t, "SISMEMBER", "s", "z"))
	assert.EqualValues(t, 0, run(t, "SISMEMBER", "nokey", "a"))

	assert.Equal(t, []interface{}{int64(1), int64(0), int64(1)}, run(t, "SMISMEMBER", "s", "a", "z", "b"))
	assert.Equal(t, "*2\r\n:1\r\n:0\r\n", string(rawReply(t, "SMISMEMBER", "s", "a", "z")),
		"the reply is a RESP array of integers a client can decode")
	assert.Equal(t, []interface{}{int64(0), int64(0)}, run(t, "SMISMEMBER", "nokey", "a", "b"))
	assert.Contains(t, run(t, "SMISMEMBER", "s"), "wrong number of arguments")
}

func TestSPOP(t *testing.T) {
	ResetStores()
	assert.Equal(t, constant.RespNil, rawReply(t, "SPOP", "nokey"))
	assert.Equal(t, constant.RespEmptyArray, rawReply(t, "SPOP", "nokey", "2"))

	run(t, "SADD", "s", "a", "b", "c")
	one := run(t, "SPOP", "s").(string)
	assert.Contains(t, []string{"a", "b", "c"}, one)
	assert.EqualValues(t, 0, run(t, "SISMEMBER", "s", one), "popped means gone")

	two := run(t, "SPOP", "s", "5").([]interface{})
	assert.Len(t, two, 2, "asking for more than there is pops what there is")
	assert.EqualValues(t, 0, run(t, "EXISTS", "s"), "and an emptied set is removed")

	run(t, "SADD", "s", "a")
	assert.Equal(t, []interface{}{}, run(t, "SPOP", "s", "0"))
	assert.EqualValues(t, 1, run(t, "SCARD", "s"))

	assert.Contains(t, run(t, "SPOP", "s", "-1"), "must be positive")
	assert.Contains(t, run(t, "SPOP", "s", "many"), "not an integer")
	assert.Contains(t, run(t, "SPOP", "s", "1", "2"), "wrong number of arguments")
}

func TestSRANDMEMBER(t *testing.T) {
	ResetStores()
	assert.Equal(t, constant.RespNil, rawReply(t, "SRANDMEMBER", "nokey"))
	assert.Equal(t, constant.RespEmptyArray, rawReply(t, "SRANDMEMBER", "nokey", "3"))

	run(t, "SADD", "s", "a", "b", "c")
	one := run(t, "SRANDMEMBER", "s").(string)
	assert.Contains(t, []string{"a", "b", "c"}, one)
	assert.EqualValues(t, 3, run(t, "SCARD", "s"), "reading removes nothing")

	two := run(t, "SRANDMEMBER", "s", "2").([]interface{})
	assert.Len(t, two, 2)
	assert.NotEqual(t, two[0], two[1], "a positive count gives distinct members")

	all := run(t, "SRANDMEMBER", "s", "10").([]interface{})
	assert.Len(t, all, 3, "no more than the set holds")

	repeats := run(t, "SRANDMEMBER", "s", "-10").([]interface{})
	assert.Len(t, repeats, 10, "a negative count gives exactly that many, repeats allowed")
	for _, m := range repeats {
		assert.Contains(t, []interface{}{"a", "b", "c"}, m)
	}

	assert.Equal(t, []interface{}{}, run(t, "SRANDMEMBER", "s", "0"))
	assert.Contains(t, run(t, "SRANDMEMBER", "s", "x"), "not an integer")
	// A negative count sizes the reply before it is written, so one no client
	// could consume is refused rather than allocated.
	assert.Contains(t, run(t, "SRANDMEMBER", "s", "-100000000000"), "out of range")
	assert.Len(t, run(t, "SRANDMEMBER", "s", "100000000000").([]interface{}), 3, "a positive count is only ever the set")
	assert.Contains(t, run(t, "SRANDMEMBER", "s", "1", "2"), "wrong number of arguments")
}

func TestSRANDIsTheOldNameForSRANDMEMBER(t *testing.T) {
	ResetStores()
	run(t, "SADD", "s", "a")
	assert.Equal(t, "a", run(t, "SRAND", "s"))
	assert.Equal(t, []interface{}{"a"}, run(t, "SRAND", "s", "1"))
}
