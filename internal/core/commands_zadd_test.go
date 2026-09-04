package core

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/brandopakel/keel/internal/constant"
)

func TestZADDCountsAdditions(t *testing.T) {
	ResetStores()
	assert.EqualValues(t, 2, run(t, "ZADD", "z", "1", "a", "2", "b"))
	assert.EqualValues(t, 0, run(t, "ZADD", "z", "5", "a"), "a rescore is not an addition")
	assert.EqualValues(t, 1, run(t, "ZADD", "z", "CH", "6", "a"), "unless CH asks for changes")
	assert.EqualValues(t, 0, run(t, "ZADD", "z", "CH", "6", "a"), "and the same score is not a change")
	assert.EqualValues(t, 2, run(t, "ZADD", "z", "CH", "7", "a", "8", "c"), "CH counts additions too")
	assert.EqualValues(t, 3, run(t, "ZCARD", "z"))
}

func TestZADDFlags(t *testing.T) {
	ResetStores()
	run(t, "ZADD", "z", "1", "a")
	assert.EqualValues(t, 0, run(t, "ZADD", "z", "NX", "9", "a"))
	assert.Equal(t, "1", run(t, "ZSCORE", "z", "a"), "NX left the score alone")
	assert.EqualValues(t, 1, run(t, "ZADD", "z", "nx", "2", "b"), "flags are case-insensitive")

	assert.EqualValues(t, 0, run(t, "ZADD", "z", "XX", "3", "c"))
	assert.Equal(t, constant.RespNil, rawReply(t, "ZSCORE", "z", "c"), "XX added nothing")
	assert.EqualValues(t, 1, run(t, "ZADD", "z", "XX", "CH", "4", "a"))
	assert.Equal(t, "4", run(t, "ZSCORE", "z", "a"))

	assert.EqualValues(t, 0, run(t, "ZADD", "fresh", "XX", "1", "a"))
	assert.EqualValues(t, 0, run(t, "EXISTS", "fresh"), "XX on a missing key leaves no key behind")

	assert.Contains(t, run(t, "ZADD", "z", "NX", "XX", "1", "a"), "XX and NX")
}

func TestZADDRefusesBadPairs(t *testing.T) {
	ResetStores()
	assert.Contains(t, run(t, "ZADD", "z", "1"), "wrong number of arguments")
	assert.Contains(t, run(t, "ZADD", "z", "1", "a", "2"), "syntax error")
	assert.Contains(t, run(t, "ZADD", "z", "CH"), "wrong number of arguments")
	assert.Contains(t, run(t, "ZADD", "z", "NX", "CH", "XX"), "XX and NX")
	assert.Contains(t, run(t, "ZADD", "z", "one", "a"), "not a valid float")
	assert.Contains(t, run(t, "ZADD", "z", "nan", "a"), "not a valid float")
	// A bad score anywhere in the batch stores none of it.
	assert.Contains(t, run(t, "ZADD", "z", "1", "a", "bad", "b"), "not a valid float")
	assert.EqualValues(t, 0, run(t, "EXISTS", "z"))

	assert.EqualValues(t, 2, run(t, "ZADD", "z", "inf", "top", "-inf", "bottom"), "infinities are scores")
	assert.EqualValues(t, 0, run(t, "ZRANK", "z", "bottom"))
	assert.EqualValues(t, 1, run(t, "ZRANK", "z", "top"))
}

func TestZRANK(t *testing.T) {
	ResetStores()
	run(t, "ZADD", "z", "30", "c", "10", "a", "20", "b", "20", "bb")
	assert.EqualValues(t, 0, run(t, "ZRANK", "z", "a"))
	assert.EqualValues(t, 1, run(t, "ZRANK", "z", "b"))
	assert.EqualValues(t, 2, run(t, "ZRANK", "z", "bb"), "equal scores rank by member")
	assert.EqualValues(t, 3, run(t, "ZRANK", "z", "c"))
	assert.Equal(t, constant.RespNil, rawReply(t, "ZRANK", "z", "nobody"), "no member, no rank")
	assert.Equal(t, constant.RespNil, rawReply(t, "ZRANK", "nokey", "a"))
	assert.Contains(t, run(t, "ZRANK", "z"), "wrong number of arguments")
}

func TestZREMDropsAnEmptiedKey(t *testing.T) {
	ResetStores()
	run(t, "ZADD", "z", "1", "a", "2", "b", "3", "c")
	assert.EqualValues(t, 2, run(t, "ZREM", "z", "a", "nobody", "c"))
	assert.EqualValues(t, 1, run(t, "ZCARD", "z"))
	assert.EqualValues(t, 1, run(t, "ZREM", "z", "b"))
	assert.EqualValues(t, 0, run(t, "EXISTS", "z"), "the last member gone, the key goes with it")
	assert.EqualValues(t, 0, run(t, "ZREM", "z", "b"))
	assert.Contains(t, run(t, "ZREM", "z"), "wrong number of arguments")
}

func TestZSCOREPrintsTheScoreItHolds(t *testing.T) {
	ResetStores()
	run(t, "ZADD", "z", "0.0000001", "tiny", "inf", "top", "-inf", "bottom", "3479099956230698", "geo", "20.5", "half")
	assert.Equal(t, "0.0000001", run(t, "ZSCORE", "z", "tiny"), "a fixed number of decimals would print zero")
	assert.Equal(t, "inf", run(t, "ZSCORE", "z", "top"), "infinities by name, as Redis prints them")
	assert.Equal(t, "-inf", run(t, "ZSCORE", "z", "bottom"))
	assert.Equal(t, "3479099956230698", run(t, "ZSCORE", "z", "geo"), "an integral score has no decimals")
	assert.Equal(t, "20.5", run(t, "ZSCORE", "z", "half"))
}

// TestNaNIsRefusedEverywhereAScoreCanEnter. NaN parses as a float and compares
// false against everything, so a member stored under it could never be found
// again; every way in has to refuse it.
func TestNaNIsRefusedEverywhereAScoreCanEnter(t *testing.T) {
	ResetStores()
	assert.Contains(t, run(t, "ZADD", "z", "nan", "m"), "not a valid float")
	assert.Contains(t, run(t, "GEOADD", "g", "nan", "1", "m"), "not a valid float")
	assert.Contains(t, run(t, "GEOADD", "g", "1", "NaN", "m"), "not a valid float")
	assert.Contains(t, run(t, "GEOSEARCH", "g", "FROMLONLAT", "1", "1", "BYRADIUS", "nan", "km"), "need numeric radius")
	assert.Contains(t, run(t, "GEOSEARCH", "g", "FROMLONLAT", "nan", "1", "BYRADIUS", "1", "km"), "not a valid float")
	assert.Contains(t, run(t, "GEOSEARCH", "g", "FROMLONLAT", "1", "1", "BYBOX", "1", "nan", "km"), "need numeric height")
	_, err := parseScore("nan")
	assert.Error(t, err, "the restore path parses scores of its own")
	_, err = parseScore("NaN")
	assert.Error(t, err)
	assert.EqualValues(t, 0, run(t, "EXISTS", "z"))
	assert.EqualValues(t, 0, run(t, "EXISTS", "g"))
}

func TestZCARD(t *testing.T) {
	ResetStores()
	assert.EqualValues(t, 0, run(t, "ZCARD", "z"))
	run(t, "ZADD", "z", "1", "a", "2", "b")
	assert.EqualValues(t, 2, run(t, "ZCARD", "z"))
	assert.Contains(t, run(t, "ZCARD"), "wrong number of arguments")
}
