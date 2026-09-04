package core

import (
	"fmt"
	"math"
	"math/rand"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/brandopakel/keel/internal/constant"
	"github.com/brandopakel/keel/internal/data_structure"
)

// The figures are the Redis documentation's examples for the geo commands, so
// the checks are against an independent implementation.

func addSicily(t *testing.T) {
	t.Helper()
	ResetStores()
	assert.EqualValues(t, 2, run(t, "GEOADD", "Sicily",
		"13.361389", "38.115556", "Palermo",
		"15.087269", "37.502669", "Catania"))
}

func TestGEOADDCountsNewMembersOnly(t *testing.T) {
	addSicily(t)
	assert.EqualValues(t, 0, run(t, "GEOADD", "Sicily", "13.361389", "38.115556", "Palermo"),
		"the same position again is not an addition")
	assert.EqualValues(t, 0, run(t, "GEOADD", "Sicily", "13.4", "38.2", "Palermo"),
		"moving a member is not an addition either")
	assert.EqualValues(t, 1, run(t, "GEOADD", "Sicily", "CH", "13.5", "38.3", "Palermo"),
		"CH counts a move")
	assert.EqualValues(t, 0, run(t, "GEOADD", "Sicily", "NX", "13.6", "38.4", "Palermo"),
		"NX leaves an existing member where it was")
	assert.EqualValues(t, 0, run(t, "GEOADD", "Sicily", "XX", "14", "37", "Messina"),
		"XX adds nothing new")
	assert.EqualValues(t, 2, run(t, "ZCARD", "Sicily"))
	assert.EqualValues(t, 0, run(t, "GEOADD", "nowhere", "XX", "14", "37", "Messina"))
	assert.EqualValues(t, 0, run(t, "EXISTS", "nowhere"), "XX on a missing key creates nothing")
}

func TestGEOADDRefusesBadInput(t *testing.T) {
	ResetStores()
	assert.Contains(t, run(t, "GEOADD", "k", "13.361389", "38.115556"), "wrong number of arguments")
	assert.Contains(t, run(t, "GEOADD", "k", "13.361389", "38.115556", "a", "1"), "syntax error",
		"positions come in threes")
	assert.Contains(t, run(t, "GEOADD", "k", "NX", "XX", "1", "1", "a"), "XX and NX")
	assert.Contains(t, run(t, "GEOADD", "k", "1", "91", "a"), "invalid longitude,latitude pair")
	assert.Contains(t, run(t, "GEOADD", "k", "181", "1", "a"), "invalid longitude,latitude pair")
	assert.Contains(t, run(t, "GEOADD", "k", "x", "1", "a"), "not a valid float")
	// One bad pair in a batch stores none of the batch.
	assert.Contains(t, run(t, "GEOADD", "k", "1", "1", "a", "1", "99", "b"), "invalid")
	assert.EqualValues(t, 0, run(t, "EXISTS", "k"))
}

func TestGEODIST(t *testing.T) {
	addSicily(t)
	assert.Equal(t, "166274.1516", run(t, "GEODIST", "Sicily", "Palermo", "Catania"))
	assert.Equal(t, "166.2742", run(t, "GEODIST", "Sicily", "Palermo", "Catania", "km"))
	assert.Equal(t, "103.3182", run(t, "GEODIST", "Sicily", "Palermo", "Catania", "mi"))
	ft, _ := strconv.ParseFloat(run(t, "GEODIST", "Sicily", "Palermo", "Catania", "FT").(string), 64)
	assert.InDelta(t, 166274.1516/0.3048, ft, 0.01)

	assert.Equal(t, constant.RespNil, rawReply(t, "GEODIST", "Sicily", "Palermo", "Foo"))
	assert.Equal(t, constant.RespNil, rawReply(t, "GEODIST", "nosuchkey", "Palermo", "Catania"))
	assert.Contains(t, run(t, "GEODIST", "Sicily", "Palermo", "Catania", "furlongs"), "unsupported unit")
	assert.Contains(t, run(t, "GEODIST", "Sicily", "Palermo"), "wrong number of arguments")
}

func TestGEOHASH(t *testing.T) {
	addSicily(t)
	assert.Equal(t, []interface{}{"sqc8b49rny0", "sqdtr74hyu0"},
		run(t, "GEOHASH", "Sicily", "Palermo", "Catania"))
	assert.Equal(t, "*3\r\n$11\r\nsqc8b49rny0\r\n$-1\r\n$11\r\nsqdtr74hyu0\r\n",
		string(rawReply(t, "GEOHASH", "Sicily", "Palermo", "nobody", "Catania")),
		"a missing member is a null in its position")
	assert.Equal(t, "*2\r\n$-1\r\n$-1\r\n", string(rawReply(t, "GEOHASH", "nosuchkey", "a", "b")))
	assert.Equal(t, constant.RespEmptyArray, rawReply(t, "GEOHASH", "Sicily"))
}

func TestGEOPOS(t *testing.T) {
	addSicily(t)
	res := run(t, "GEOPOS", "Sicily", "Palermo", "NonExisting", "Catania").([]interface{})
	assert.Len(t, res, 3)

	palermo := res[0].([]interface{})
	long, _ := strconv.ParseFloat(palermo[0].(string), 64)
	lat, _ := strconv.ParseFloat(palermo[1].(string), 64)
	assert.InDelta(t, 13.361389, long, 1e-5, "positions come back to within the index's resolution")
	assert.InDelta(t, 38.115556, lat, 1e-5)

	assert.Nil(t, res[1], "a missing member is a null array")
	assert.Equal(t, "*1\r\n*-1\r\n", string(rawReply(t, "GEOPOS", "Sicily", "nobody")))
	assert.Equal(t, "*1\r\n*-1\r\n", string(rawReply(t, "GEOPOS", "nosuchkey", "nobody")))
}

func TestGEOSEARCHByRadiusAndBox(t *testing.T) {
	addSicily(t)
	assert.Equal(t, []interface{}{"Catania", "Palermo"},
		run(t, "GEOSEARCH", "Sicily", "FROMLONLAT", "15", "37", "BYRADIUS", "200", "km", "ASC"))
	assert.Equal(t, []interface{}{"Palermo", "Catania"},
		run(t, "GEOSEARCH", "Sicily", "FROMLONLAT", "15", "37", "BYRADIUS", "200", "km", "DESC"))
	assert.ElementsMatch(t, []interface{}{"Catania", "Palermo"},
		run(t, "GEOSEARCH", "Sicily", "FROMLONLAT", "15", "37", "BYRADIUS", "200", "km"),
		"no order asked, so any order")

	// The documentation's BYBOX example, with its distances.
	res := run(t, "GEOSEARCH", "Sicily", "FROMLONLAT", "15", "37", "BYBOX", "400", "400", "km",
		"ASC", "WITHCOORD", "WITHDIST").([]interface{})
	assert.Len(t, res, 2)
	catania := res[0].([]interface{})
	assert.Equal(t, "Catania", catania[0])
	assert.Equal(t, "56.4413", catania[1])
	coords := catania[2].([]interface{})
	long, _ := strconv.ParseFloat(coords[0].(string), 64)
	assert.InDelta(t, 15.087269, long, 1e-5)
	palermo := res[1].([]interface{})
	assert.Equal(t, "Palermo", palermo[0])
	assert.Equal(t, "190.4424", palermo[1])

	// Every option at once, in the order the reply carries them.
	res = run(t, "GEOSEARCH", "Sicily", "FROMMEMBER", "Palermo", "BYRADIUS", "1", "m",
		"WITHHASH", "WITHDIST", "WITHCOORD").([]interface{})
	assert.Len(t, res, 1)
	entry := res[0].([]interface{})
	assert.Equal(t, "Palermo", entry[0])
	assert.Equal(t, "0.0000", entry[1])
	assert.EqualValues(t, 3479099956230698, entry[2])
	assert.Len(t, entry[3], 2)

	// A radius that reaches nobody is an empty array, not nil.
	assert.Equal(t, constant.RespEmptyArray, rawReply(t, "GEOSEARCH", "Sicily", "FROMLONLAT", "0", "0", "BYRADIUS", "1", "km"))
	assert.Equal(t, constant.RespEmptyArray, rawReply(t, "GEOSEARCH", "nosuchkey", "FROMLONLAT", "0", "0", "BYRADIUS", "1", "km"))
}

func TestGEOSEARCHCount(t *testing.T) {
	addSicily(t)
	assert.Equal(t, []interface{}{"Catania"},
		run(t, "GEOSEARCH", "Sicily", "FROMLONLAT", "15", "37", "BYRADIUS", "200", "km", "COUNT", "1"),
		"COUNT without an order means the nearest")
	assert.Equal(t, []interface{}{"Palermo"},
		run(t, "GEOSEARCH", "Sicily", "FROMLONLAT", "15", "37", "BYRADIUS", "200", "km", "COUNT", "1", "DESC"))
	any := run(t, "GEOSEARCH", "Sicily", "FROMLONLAT", "15", "37", "BYRADIUS", "200", "km", "COUNT", "1", "ANY").([]interface{})
	assert.Len(t, any, 1)
	assert.Contains(t, []interface{}{"Catania", "Palermo"}, any[0])

	assert.Contains(t, run(t, "GEOSEARCH", "Sicily", "FROMLONLAT", "15", "37", "BYRADIUS", "200", "km", "COUNT", "0"), "COUNT must be > 0")
	assert.Contains(t, run(t, "GEOSEARCH", "Sicily", "FROMLONLAT", "15", "37", "BYRADIUS", "200", "km", "ANY"), "ANY argument requires COUNT")
}

func TestGEOSEARCHRefusesHalfAQuestion(t *testing.T) {
	addSicily(t)
	assert.Contains(t, run(t, "GEOSEARCH", "Sicily", "BYRADIUS", "200", "km"), "FROMMEMBER or FROMLONLAT")
	assert.Contains(t, run(t, "GEOSEARCH", "Sicily", "FROMLONLAT", "15", "37"), "BYRADIUS and BYBOX")
	assert.Contains(t, run(t, "GEOSEARCH", "Sicily", "FROMLONLAT", "15", "37", "BYRADIUS", "200", "leagues"), "unsupported unit")
	assert.Contains(t, run(t, "GEOSEARCH", "Sicily", "FROMLONLAT", "15", "37", "BYRADIUS", "-1", "km"), "cannot be negative")
	assert.Contains(t, run(t, "GEOSEARCH", "Sicily", "FROMLONLAT", "15", "37", "BYRADIUS", "abc", "km"), "need numeric radius")
	assert.Contains(t, run(t, "GEOSEARCH", "Sicily", "FROMLONLAT", "15", "37", "FROMMEMBER", "Palermo", "BYRADIUS", "1", "km"), "syntax error",
		"one centre only")
	assert.Contains(t, run(t, "GEOSEARCH", "Sicily", "FROMLONLAT", "15", "37", "BYRADIUS", "1", "km", "BYBOX", "1", "1", "km"), "syntax error",
		"one extent only")
	assert.Contains(t, run(t, "GEOSEARCH", "Sicily", "FROMLONLAT", "15", "37", "BYRADIUS", "1", "km", "SIDEWAYS"), "syntax error")
	assert.Contains(t, run(t, "GEOSEARCH", "Sicily", "FROMMEMBER", "nobody", "BYRADIUS", "1", "km"), "could not decode requested zset member")
	assert.Contains(t, run(t, "GEOSEARCH", "Sicily"), "wrong number of arguments")
}

// TestGEOSEARCHOldRadiusForm: before it took the Redis form, this server's
// GEOSEARCH ended in a bare radius in metres. A log written then still replays.
func TestGEOSEARCHOldRadiusForm(t *testing.T) {
	addSicily(t)
	assert.ElementsMatch(t, []interface{}{"Catania", "Palermo"},
		run(t, "GEOSEARCH", "Sicily", "FROMLONLAT", "15", "37", "200000"))
	assert.Equal(t, []interface{}{"Palermo"},
		run(t, "GEOSEARCH", "Sicily", "FROMMEMBER", "Palermo", "1000"))
	assert.Contains(t, run(t, "GEOSEARCH", "Sicily", "FROMLONLAT", "15", "37", "-5"), "syntax error",
		"a negative number is not a radius")
}

// TestGEOSEARCHAgreesWithBruteForce checks the search end to end against a
// plain distance check over every member.
func TestGEOSEARCHAgreesWithBruteForce(t *testing.T) {
	ResetStores()
	rng := rand.New(rand.NewSource(9))
	type pos struct{ long, lat float64 }
	points := map[string]pos{}
	for i := 0; i < 3000; i++ {
		p := pos{-180 + rng.Float64()*360, -85 + rng.Float64()*170}
		name := fmt.Sprintf("m%d", i)
		points[name] = p
		run(t, "GEOADD", "world", fmt.Sprintf("%f", p.long), fmt.Sprintf("%f", p.lat), name)
	}
	assert.EqualValues(t, 3000, run(t, "ZCARD", "world"))

	for round := 0; round < 20; round++ {
		cLong := -180 + rng.Float64()*360
		cLat := -80 + rng.Float64()*160
		radiusKm := math.Pow(10, 1+rng.Float64()*3.5)

		var want []interface{}
		for name := range points {
			// The stored position is the centre of the member's box, so the
			// reference measures from there too.
			stored, _ := run(t, "GEOPOS", "world", name).([]interface{})
			coords := stored[0].([]interface{})
			sLong, _ := strconv.ParseFloat(coords[0].(string), 64)
			sLat, _ := strconv.ParseFloat(coords[1].(string), 64)
			if data_structure.GeohashGetDistance(cLong, cLat, sLong, sLat) <= radiusKm*1000 {
				want = append(want, name)
			}
		}
		got := run(t, "GEOSEARCH", "world", "FROMLONLAT",
			strconv.FormatFloat(cLong, 'f', -1, 64), strconv.FormatFloat(cLat, 'f', -1, 64),
			"BYRADIUS", strconv.FormatFloat(radiusKm, 'f', -1, 64), "km")
		if want == nil {
			assert.Equal(t, constant.RespEmptyArray, rawReply(t, "GEOSEARCH", "world", "FROMLONLAT",
				strconv.FormatFloat(cLong, 'f', -1, 64), strconv.FormatFloat(cLat, 'f', -1, 64),
				"BYRADIUS", strconv.FormatFloat(radiusKm, 'f', -1, 64), "km"))
			continue
		}
		assert.ElementsMatch(t, want, got, "round %d: %f,%f r=%fkm", round, cLong, cLat, radiusKm)
	}
}
