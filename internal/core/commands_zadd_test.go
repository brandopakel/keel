package core

import (
	"strconv"
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

// TestZSCOREPrintsScoresAsRedisDoes pins the layout to Redis 7.2's d2string
// and fpconv_dtoa, whose rules the cases below were worked from.
func TestZSCOREPrintsScoresAsRedisDoes(t *testing.T) {
	ResetStores()
	cases := map[string]string{
		"20.5":                 "20.5",
		"1234.5678":            "1234.5678",
		"0.1":                  "0.1",
		"-0.5":                 "-0.5",
		"0.000001":             "0.000001",
		"0.0000001":            "1e-7",
		"1e-8":                 "1e-8",
		"1e100":                "1e+100",
		"1.5e300":              "1.5e+300",
		"-1.5e-300":            "-1.5e-300",
		"0.30000000000000004":  "0.30000000000000004",
		"3479099956230698":     "3479099956230698",
		"10000000000000000":    "10000000000000000",
		"-42":                  "-42",
		"1e19":                 "1e+19",
		"12345678901234567890": "12345678901234567000",
		"inf":                  "inf",
		"-inf":                 "-inf",
		"0":                    "0",
		"-0":                   "-0",
	}
	i := 0
	for score, want := range cases {
		member := "m" + strconv.Itoa(i)
		i++
		run(t, "ZADD", "z", score, member)
		assert.Equal(t, want, run(t, "ZSCORE", "z", member), "score %s", score)
	}
	assert.Equal(t, "1e-7", formatZScore(1e-7))
	// Variables rather than constants: Go folds 0.1+0.2 exactly to 0.3 at
	// compile time, and the point is the float64 sum.
	tenth, fifth := 0.1, 0.2
	assert.Equal(t, "0.30000000000000004", formatZScore(tenth+fifth))
}

// packParts builds the body of a set or sorted set dump payload.
func packParts(parts ...string) []byte {
	w := &respParts{}
	for _, p := range parts {
		w.add(p)
	}
	return w.encode()
}

// TestRestoreLeavesTheOldValueWhenThePayloadIsBad. The restore path decodes
// the whole payload before it touches the key; before it did, a payload that
// failed halfway had already deleted what was there.
func TestRestoreLeavesTheOldValueWhenThePayloadIsBad(t *testing.T) {
	ResetStores()
	run(t, "ZADD", "z", "1", "keep")
	run(t, "SET", "s", "value")

	bad := append([]byte{dumpTagZSet}, packParts("nan", "m")...)
	assert.Contains(t, run(t, "KEEL.RESTORE", "z", string(bad)), "bad score")
	assert.Equal(t, "1", run(t, "ZSCORE", "z", "keep"), "the sorted set is as it was")

	assert.Contains(t, run(t, "KEEL.RESTORE", "s", string(bad)), "bad score")
	assert.Equal(t, "value", run(t, "GET", "s"), "a key of another type is as it was too")

	odd := append([]byte{dumpTagZSet}, packParts("1", "m", "2")...)
	assert.Contains(t, run(t, "KEEL.RESTORE", "z", string(odd)), "score/member pairs")
	assert.Equal(t, "1", run(t, "ZSCORE", "z", "keep"))

	assert.Contains(t, run(t, "KEEL.RESTORE", "z", string([]byte{200})), "unknown payload type")
	assert.Equal(t, "1", run(t, "ZSCORE", "z", "keep"))
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
