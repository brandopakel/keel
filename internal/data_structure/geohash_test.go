package data_structure

import (
	"math"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The positions and figures below are the ones in the Redis documentation for
// GEOADD, GEODIST, GEOHASH and GEOSEARCH, which makes them a check against an
// independent implementation rather than against this one.
const (
	palermoLong, palermoLat = 13.361389, 38.115556
	cataniaLong, cataniaLat = 15.087269, 37.502669
	palermoScore            = 3479099956230698
	cataniaScore            = 3479447370796909
	palermoCataniaMeters    = 166274.1516
)

func TestInterleaveRoundTrips(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 1000; i++ {
		x, y := rng.Uint32(), rng.Uint32()
		back := deinterleave64(interleave64(x, y))
		assert.Equal(t, x, uint32(back), "even bits carry x")
		assert.Equal(t, y, uint32(back>>32), "odd bits carry y")
	}
	assert.EqualValues(t, 0b0101, interleave64(0b11, 0), "x lands in the even positions")
	assert.EqualValues(t, 0b1010, interleave64(0, 0b11), "y lands in the odd positions")
}

func TestGeoScoreMatchesRedis(t *testing.T) {
	score, ok := GeoScore(palermoLong, palermoLat)
	assert.True(t, ok)
	assert.EqualValues(t, palermoScore, score)
	score, ok = GeoScore(cataniaLong, cataniaLat)
	assert.True(t, ok)
	assert.EqualValues(t, cataniaScore, score)
}

func TestGeohashStringMatchesRedis(t *testing.T) {
	s, ok := GeohashString(palermoLong, palermoLat)
	assert.True(t, ok)
	assert.Equal(t, "sqc8b49rny0", s)
	s, ok = GeohashString(cataniaLong, cataniaLat)
	assert.True(t, ok)
	assert.Equal(t, "sqdtr74hyu0", s)
	_, ok = GeohashString(0, 89)
	assert.False(t, ok, "a latitude the index cannot hold has no hash")
}

func TestGeohashGetDistanceMatchesRedis(t *testing.T) {
	// The documented figure is between the positions as stored, which are the
	// centres of the boxes the inputs fall in, not the inputs themselves.
	pScore, _ := GeoScore(palermoLong, palermoLat)
	cScore, _ := GeoScore(cataniaLong, cataniaLat)
	pLong, pLat, _ := GeoDecodeScore(float64(pScore))
	cLong, cLat, _ := GeoDecodeScore(float64(cScore))
	d := GeohashGetDistance(pLong, pLat, cLong, cLat)
	assert.InDelta(t, palermoCataniaMeters, d, 0.0001)
	// From the raw inputs it is a few centimetres off, which is the index's
	// resolution showing.
	assert.InDelta(t, palermoCataniaMeters, GeohashGetDistance(palermoLong, palermoLat, cataniaLong, cataniaLat), 0.5)
	assert.Zero(t, GeohashGetDistance(1, 2, 1, 2))
	// Along a meridian the formula takes its short cut, and must agree with
	// the long way round.
	assert.InDelta(t, degToRad(10)*earthRadiusInMeters, GeohashGetDistance(5, 0, 5, 10), 1e-6)
}

func TestGeohashEncodeDecodeRoundTrips(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	for i := 0; i < 2000; i++ {
		long := GeoLongMin + rng.Float64()*(GeoLongMax-GeoLongMin)
		lat := GeoLatMin + rng.Float64()*(GeoLatMax-GeoLatMin)

		hash, ok := GeohashEncodeWGS84(long, lat, GeoStepMax)
		assert.True(t, ok)
		area, ok := GeohashDecodeWGS84(hash)
		assert.True(t, ok)
		assert.LessOrEqual(t, area.Longitude.Min, long)
		assert.GreaterOrEqual(t, area.Longitude.Max, long)
		assert.LessOrEqual(t, area.Latitude.Min, lat)
		assert.GreaterOrEqual(t, area.Latitude.Max, lat)

		// 52 bits resolve to well under a metre.
		backLong, backLat, ok := GeohashDecodeToLongLatWGS84(hash)
		assert.True(t, ok)
		assert.Less(t, GeohashGetDistance(long, lat, backLong, backLat), 1.0)

		// And the score is the same position.
		score, _ := GeoScore(long, lat)
		sLong, sLat, ok := GeoDecodeScore(float64(score))
		assert.True(t, ok)
		assert.Equal(t, backLong, sLong)
		assert.Equal(t, backLat, sLat)
	}
}

func TestGeohashEncodeRefusesWhatItCannotIndex(t *testing.T) {
	longRange, latRange := GeohashCoordRange()
	_, ok := GeohashEncode(longRange, latRange, 0, 90, 26)
	assert.False(t, ok, "the poles are outside Web Mercator")
	_, ok = GeohashEncode(longRange, latRange, 181, 0, 26)
	assert.False(t, ok)
	_, ok = GeohashEncode(longRange, latRange, 0, 0, 0)
	assert.False(t, ok, "zero steps")
	_, ok = GeohashEncode(longRange, latRange, 0, 0, 33)
	assert.False(t, ok, "more steps than fit")
	_, ok = GeohashEncode(GeoHashRange{}, latRange, 0, 0, 26)
	assert.False(t, ok, "zero range")
	_, ok = GeohashDecodeWGS84(GeoHashBits{})
	assert.False(t, ok, "the zero hash stands for no area")
}

func TestGeohashAlign52Bits(t *testing.T) {
	assert.EqualValues(t, 0b1011<<48, GeohashAlign52Bits(GeoHashBits{Bits: 0b1011, Step: 2}))
	assert.EqualValues(t, 12345, GeohashAlign52Bits(GeoHashBits{Bits: 12345, Step: 26}), "a full hash is already aligned")
}

func TestGeohashNeighborsAreAdjacentBoxes(t *testing.T) {
	longRange, latRange := GeohashCoordRange()
	hash, _ := GeohashEncodeWGS84(palermoLong, palermoLat, 10)
	area, _ := GeohashDecode(longRange, latRange, hash)
	n := GeohashNeighbors(hash)

	east, _ := GeohashDecode(longRange, latRange, n.East)
	assert.InDelta(t, area.Longitude.Max, east.Longitude.Min, 1e-9)
	assert.Equal(t, area.Latitude, east.Latitude)

	west, _ := GeohashDecode(longRange, latRange, n.West)
	assert.InDelta(t, area.Longitude.Min, west.Longitude.Max, 1e-9)

	north, _ := GeohashDecode(longRange, latRange, n.North)
	assert.InDelta(t, area.Latitude.Max, north.Latitude.Min, 1e-9)
	assert.Equal(t, area.Longitude, north.Longitude)

	south, _ := GeohashDecode(longRange, latRange, n.South)
	assert.InDelta(t, area.Latitude.Min, south.Latitude.Max, 1e-9)

	ne, _ := GeohashDecode(longRange, latRange, n.NorthEast)
	assert.InDelta(t, area.Longitude.Max, ne.Longitude.Min, 1e-9)
	assert.InDelta(t, area.Latitude.Max, ne.Latitude.Min, 1e-9)

	sw, _ := GeohashDecode(longRange, latRange, n.SouthWest)
	assert.InDelta(t, area.Longitude.Min, sw.Longitude.Max, 1e-9)
	assert.InDelta(t, area.Latitude.Min, sw.Latitude.Max, 1e-9)

	// Moving back returns to the same box.
	back := n.East
	geohashMoveX(&back, -1)
	assert.Equal(t, hash, back)
	back = n.North
	geohashMoveY(&back, -1)
	assert.Equal(t, hash, back)
}

func TestGeohashEstimateStepsByRadius(t *testing.T) {
	assert.EqualValues(t, 26, GeohashEstimateStepsByRadius(0, 0))
	assert.EqualValues(t, 1, GeohashEstimateStepsByRadius(mercatorMax*4, 0), "the whole world is one box")

	prev := GeohashEstimateStepsByRadius(1, 0)
	for r := 2.0; r < 1e8; r *= 2 {
		step := GeohashEstimateStepsByRadius(r, 0)
		assert.LessOrEqual(t, step, prev, "a wider search never wants a finer step")
		prev = step
	}
	equator := GeohashEstimateStepsByRadius(5000, 0)
	assert.Equal(t, equator-1, GeohashEstimateStepsByRadius(5000, 70), "boxes narrow towards the poles")
	assert.Equal(t, equator-2, GeohashEstimateStepsByRadius(5000, 84))
}

// TestGeohashAreasCoverTheSearch checks the property the search relies on: the
// boxes chosen for a shape, taken together, reach at least as far as the shape
// does in every direction.
func TestGeohashAreasCoverTheSearch(t *testing.T) {
	longRange, latRange := GeohashCoordRange()
	rng := rand.New(rand.NewSource(11))
	for i := 0; i < 500; i++ {
		shape := &GeoShape{
			Type:       GeoShapeCircle,
			Longitude:  -180 + rng.Float64()*360,
			Latitude:   -80 + rng.Float64()*160,
			Conversion: 1,
			Radius:     math.Pow(10, 1+rng.Float64()*6), // 10m to 10,000km
		}
		if i%2 == 1 {
			shape.Type, shape.Width, shape.Height = GeoShapeBox, shape.Radius*2, shape.Radius
		}
		radius := GeohashCalculateAreasByShapeWGS84(shape)
		b := shape.Bounds

		minLong, minLat := math.Inf(1), math.Inf(1)
		maxLong, maxLat := math.Inf(-1), math.Inf(-1)
		n := radius.Neighbors
		for _, h := range []GeoHashBits{radius.Hash, n.North, n.South, n.East, n.West, n.NorthEast, n.NorthWest, n.SouthEast, n.SouthWest} {
			if h.IsZero() {
				continue
			}
			a, ok := GeohashDecode(longRange, latRange, h)
			assert.True(t, ok)
			minLong = math.Min(minLong, a.Longitude.Min)
			maxLong = math.Max(maxLong, a.Longitude.Max)
			minLat = math.Min(minLat, a.Latitude.Min)
			maxLat = math.Max(maxLat, a.Latitude.Max)
		}
		// A shape that runs off the edge of the indexable world cannot be
		// covered there, so only the edges inside it are checked.
		if b.MinLong > GeoLongMin {
			assert.LessOrEqual(t, minLong, b.MinLong, "west edge, case %d", i)
		}
		if b.MaxLong < GeoLongMax {
			assert.GreaterOrEqual(t, maxLong, b.MaxLong, "east edge, case %d", i)
		}
		if b.MinLat > GeoLatMin {
			assert.LessOrEqual(t, minLat, b.MinLat, "south edge, case %d", i)
		}
		if b.MaxLat < GeoLatMax {
			assert.GreaterOrEqual(t, maxLat, b.MaxLat, "north edge, case %d", i)
		}
	}
}

// TestGeoMembersOfAllNeighborsAgreesWithBruteForce fills a set with random
// positions and compares every search against a plain distance check over all
// of them.
func TestGeoMembersOfAllNeighborsAgreesWithBruteForce(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	type pos struct{ long, lat float64 }
	points := map[string]pos{}
	zs := CreateZSet()
	for i := 0; i < 5000; i++ {
		p := pos{-180 + rng.Float64()*360, -85 + rng.Float64()*170}
		name := "p" + itoa(i)
		points[name] = p
		score, ok := GeoScore(p.long, p.lat)
		assert.True(t, ok)
		zs.Add(float64(score), name, 0)
	}

	for round := 0; round < 40; round++ {
		shape := &GeoShape{
			Type:       GeoShapeCircle,
			Longitude:  -180 + rng.Float64()*360,
			Latitude:   -80 + rng.Float64()*160,
			Conversion: 1,
			Radius:     math.Pow(10, 3+rng.Float64()*4), // 1km to 10,000km
		}
		if round%2 == 1 {
			shape.Type, shape.Width, shape.Height = GeoShapeBox, shape.Radius*2, shape.Radius*1.5
		}

		var want []string
		for name, p := range points {
			// Distances are measured from the stored position, which is the
			// centre of the point's box, not from the exact input.
			score, _ := GeoScore(p.long, p.lat)
			sLong, sLat, _ := GeoDecodeScore(float64(score))
			if _, _, _, ok := geoWithinShape(shape, float64(score)); ok {
				_ = sLong
				_ = sLat
				want = append(want, name)
			}
		}

		radius := GeohashCalculateAreasByShapeWGS84(shape)
		found := zs.GeoMembersOfAllNeighbors(radius, shape, 0)
		var got []string
		for _, f := range found {
			got = append(got, f.Member)
			assert.Equal(t, points[f.Member].long, points[f.Member].long)
		}
		assert.ElementsMatch(t, want, got, "round %d", round)

		if len(want) > 2 {
			limited := zs.GeoMembersOfAllNeighbors(radius, shape, 2)
			assert.Len(t, limited, 2, "a limit stops the search early")
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
}
