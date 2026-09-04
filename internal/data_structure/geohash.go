/*
 * Copyright (c) 2013-2014, yinqiwen <yinqiwen@gmail.com>
 * Copyright (c) 2014, Matt Stancliff <matt@genges.com>.
 * Copyright (c) 2015-2016, Salvatore Sanfilippo <antirez@gmail.com>.
 * All rights reserved.
 *
 * Redistribution and use in source and binary forms, with or without
 * modification, are permitted provided that the following conditions are met:
 *
 *  * Redistributions of source code must retain the above copyright notice,
 *    this list of conditions and the following disclaimer.
 *  * Redistributions in binary form must reproduce the above copyright
 *    notice, this list of conditions and the following disclaimer in the
 *    documentation and/or other materials provided with the distribution.
 *  * Neither the name of Redis nor the names of its contributors may be used
 *    to endorse or promote products derived from this software without
 *    specific prior written permission.
 *
 * THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
 * AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
 * IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE
 * ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT OWNER OR CONTRIBUTORS
 * BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR
 * CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF
 * SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS
 * INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN
 * CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE)
 * ARISING IN ANY WAY OUT OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF
 * THE POSSIBILITY OF SUCH DAMAGE.
 */

// This file is a Go translation of src/geohash.c and src/geohash_helper.c from
// Redis 7.2.4, which carry the licence above; geohash_helper.c is itself a C
// conversion of the ardb project's geohash_helper.cpp. The translation was
// made for keel by Brando Pakel in 2026 and is distributed under the same
// terms.

package data_structure

import "math"

// Geohashing.
//
// A position is turned into one integer by splitting the world in four, again
// and again: each split contributes one bit for longitude and one for latitude,
// interleaved, and after step splits the integer has 2*step bits. Positions
// close together share a prefix, so a box on the map is a range of integers -
// which is what lets a radius search become a handful of range queries over a
// sorted set whose scores are these integers.
//
// The latitude limits below are those of Web Mercator rather than the poles,
// because the projection cannot represent the poles; nothing above 85 degrees
// can be indexed.

const (
	// GeoStepMax is the number of splits used for scores: 26 in each axis is
	// 52 bits, which fits exactly in a float64's mantissa, and resolves to
	// about 0.6 metres.
	GeoStepMax uint8 = 26

	// Limits from EPSG:900913 / EPSG:3785 / OSGEO:41001.
	GeoLatMin  = -85.05112878
	GeoLatMax  = 85.05112878
	GeoLongMin = -180
	GeoLongMax = 180
)

const (
	degreesToRadians = math.Pi / 180.0
	// earthRadiusInMeters is the quadratic mean radius of the WGS-84 ellipsoid.
	earthRadiusInMeters = 6372797.560856
	// mercatorMax is half the circumference of the Web Mercator world, in
	// metres, which is the widest a search box can be.
	mercatorMax = 20037726.37
)

// GeoHashBits is a hash of Step splits per axis, so 2*Step significant bits.
type GeoHashBits struct {
	Bits uint64
	Step uint8
}

// IsZero reports the zero hash, which stands for "no area" in a neighbour set.
func (h GeoHashBits) IsZero() bool { return h.Bits == 0 && h.Step == 0 }

// GeoHashRange is an interval of degrees along one axis.
type GeoHashRange struct {
	Min, Max float64
}

func (r GeoHashRange) isZero() bool { return r.Min == 0 && r.Max == 0 }

// GeoHashArea is the box on the map a hash stands for.
type GeoHashArea struct {
	Hash      GeoHashBits
	Longitude GeoHashRange
	Latitude  GeoHashRange
}

// GeoHashNeighbors are the eight boxes around a hash at the same step.
type GeoHashNeighbors struct {
	North, East, West, South                   GeoHashBits
	NorthEast, SouthEast, NorthWest, SouthWest GeoHashBits
}

// GeoHashRadius is the set of boxes a search has to look in: the box holding
// the centre and whichever of its neighbours the search area reaches into.
type GeoHashRadius struct {
	Hash      GeoHashBits
	Area      GeoHashArea
	Neighbors GeoHashNeighbors
}

// interleave64 spreads the bits of x into the even positions and the bits of y
// into the odd ones. Each round doubles the gap between a value's bits.
// From https://graphics.stanford.edu/~seander/bithacks.html#InterleaveBMN.
func interleave64(xlo, ylo uint32) uint64 {
	x, y := uint64(xlo), uint64(ylo)

	x = (x | (x << 16)) & 0x0000FFFF0000FFFF
	y = (y | (y << 16)) & 0x0000FFFF0000FFFF

	x = (x | (x << 8)) & 0x00FF00FF00FF00FF
	y = (y | (y << 8)) & 0x00FF00FF00FF00FF

	x = (x | (x << 4)) & 0x0F0F0F0F0F0F0F0F
	y = (y | (y << 4)) & 0x0F0F0F0F0F0F0F0F

	x = (x | (x << 2)) & 0x3333333333333333
	y = (y | (y << 2)) & 0x3333333333333333

	x = (x | (x << 1)) & 0x5555555555555555
	y = (y | (y << 1)) & 0x5555555555555555

	return x | (y << 1)
}

// deinterleave64 undoes interleave64: the even bits come back in the low 32
// bits of the result and the odd bits in the high 32.
func deinterleave64(interleaved uint64) uint64 {
	x := interleaved
	y := interleaved >> 1

	x = (x | (x >> 0)) & 0x5555555555555555
	y = (y | (y >> 0)) & 0x5555555555555555

	x = (x | (x >> 1)) & 0x3333333333333333
	y = (y | (y >> 1)) & 0x3333333333333333

	x = (x | (x >> 2)) & 0x0F0F0F0F0F0F0F0F
	y = (y | (y >> 2)) & 0x0F0F0F0F0F0F0F0F

	x = (x | (x >> 4)) & 0x00FF00FF00FF00FF
	y = (y | (y >> 4)) & 0x00FF00FF00FF00FF

	x = (x | (x >> 8)) & 0x0000FFFF0000FFFF
	y = (y | (y >> 8)) & 0x0000FFFF0000FFFF

	x = (x | (x >> 16)) & 0x00000000FFFFFFFF
	y = (y | (y >> 16)) & 0x00000000FFFFFFFF

	return x | (y << 32)
}

// GeohashCoordRange is the range every score is encoded against.
func GeohashCoordRange() (longRange, latRange GeoHashRange) {
	return GeoHashRange{Min: GeoLongMin, Max: GeoLongMax},
		GeoHashRange{Min: GeoLatMin, Max: GeoLatMax}
}

// GeohashEncode hashes a position against the given ranges at step splits per
// axis. It reports false for a step outside 1..32, a zero range, or a position
// outside both the supported limits and the ranges.
func GeohashEncode(longRange, latRange GeoHashRange, longitude, latitude float64, step uint8) (GeoHashBits, bool) {
	if step > 32 || step == 0 || latRange.isZero() || longRange.isZero() {
		return GeoHashBits{}, false
	}
	if longitude > GeoLongMax || longitude < GeoLongMin ||
		latitude > GeoLatMax || latitude < GeoLatMin {
		return GeoHashBits{}, false
	}
	if latitude < latRange.Min || latitude > latRange.Max ||
		longitude < longRange.Min || longitude > longRange.Max {
		return GeoHashBits{}, false
	}

	// Where the position sits in each range, as a fraction, then as a fixed
	// point number with step bits.
	latOffset := (latitude - latRange.Min) / (latRange.Max - latRange.Min)
	longOffset := (longitude - longRange.Min) / (longRange.Max - longRange.Min)
	latOffset *= float64(uint64(1) << step)
	longOffset *= float64(uint64(1) << step)

	return GeoHashBits{
		Bits: interleave64(uint32(latOffset), uint32(longOffset)),
		Step: step,
	}, true
}

// GeohashEncodeWGS84 hashes a position against the standard coordinate range.
func GeohashEncodeWGS84(longitude, latitude float64, step uint8) (GeoHashBits, bool) {
	longRange, latRange := GeohashCoordRange()
	return GeohashEncode(longRange, latRange, longitude, latitude, step)
}

// GeohashDecode returns the box a hash stands for within the given ranges. It
// reports false for the zero hash or a zero range.
func GeohashDecode(longRange, latRange GeoHashRange, hash GeoHashBits) (GeoHashArea, bool) {
	if hash.IsZero() || latRange.isZero() || longRange.isZero() {
		return GeoHashArea{}, false
	}

	// The hash is [lat][long] interleaved; pulled apart, latitude is the low
	// word and longitude the high.
	separated := deinterleave64(hash.Bits)
	ilat := uint32(separated)
	ilong := uint32(separated >> 32)

	latScale := latRange.Max - latRange.Min
	longScale := longRange.Max - longRange.Min
	cells := float64(uint64(1) << hash.Step)

	return GeoHashArea{
		Hash: hash,
		Latitude: GeoHashRange{
			Min: latRange.Min + (float64(ilat)/cells)*latScale,
			Max: latRange.Min + (float64(ilat+1)/cells)*latScale,
		},
		Longitude: GeoHashRange{
			Min: longRange.Min + (float64(ilong)/cells)*longScale,
			Max: longRange.Min + (float64(ilong+1)/cells)*longScale,
		},
	}, true
}

// GeohashDecodeWGS84 returns the box a hash stands for in the standard range.
func GeohashDecodeWGS84(hash GeoHashBits) (GeoHashArea, bool) {
	longRange, latRange := GeohashCoordRange()
	return GeohashDecode(longRange, latRange, hash)
}

// Center is the middle of the area, clamped to the supported limits.
func (a GeoHashArea) Center() (longitude, latitude float64) {
	longitude = (a.Longitude.Min + a.Longitude.Max) / 2
	longitude = math.Max(GeoLongMin, math.Min(GeoLongMax, longitude))
	latitude = (a.Latitude.Min + a.Latitude.Max) / 2
	latitude = math.Max(GeoLatMin, math.Min(GeoLatMax, latitude))
	return longitude, latitude
}

// GeohashDecodeToLongLatWGS84 is the position a hash stands for: the centre of
// its box.
func GeohashDecodeToLongLatWGS84(hash GeoHashBits) (longitude, latitude float64, ok bool) {
	area, ok := GeohashDecodeWGS84(hash)
	if !ok {
		return 0, 0, false
	}
	longitude, latitude = area.Center()
	return longitude, latitude, true
}

// The longitude bits sit in the odd positions of a hash and the latitude bits
// in the even ones. Stepping one box east or north means adding one to the
// longitude or latitude value while leaving the other's bits alone, which the
// masks below arrange: the bits not being changed are filled with ones first,
// so a carry runs straight through them.

const (
	oddBits  = 0xaaaaaaaaaaaaaaaa
	evenBits = 0x5555555555555555
)

// geohashMoveX shifts the hash one box east (d > 0) or west (d < 0).
func geohashMoveX(hash *GeoHashBits, d int8) {
	if d == 0 {
		return
	}
	x := hash.Bits & oddBits
	y := hash.Bits & evenBits

	zz := uint64(evenBits) >> (64 - uint(hash.Step)*2)
	if d > 0 {
		x += zz + 1
	} else {
		x |= zz
		x -= zz + 1
	}
	x &= uint64(oddBits) >> (64 - uint(hash.Step)*2)
	hash.Bits = x | y
}

// geohashMoveY shifts the hash one box north (d > 0) or south (d < 0).
func geohashMoveY(hash *GeoHashBits, d int8) {
	if d == 0 {
		return
	}
	x := hash.Bits & oddBits
	y := hash.Bits & evenBits

	zz := uint64(oddBits) >> (64 - uint(hash.Step)*2)
	if d > 0 {
		y += zz + 1
	} else {
		y |= zz
		y -= zz + 1
	}
	y &= uint64(evenBits) >> (64 - uint(hash.Step)*2)
	hash.Bits = x | y
}

// GeohashNeighbors returns the eight boxes around hash at its step.
func GeohashNeighbors(hash GeoHashBits) GeoHashNeighbors {
	n := GeoHashNeighbors{
		North: hash, East: hash, West: hash, South: hash,
		NorthEast: hash, SouthEast: hash, NorthWest: hash, SouthWest: hash,
	}
	geohashMoveX(&n.East, 1)
	geohashMoveX(&n.West, -1)
	geohashMoveY(&n.South, -1)
	geohashMoveY(&n.North, 1)

	geohashMoveX(&n.NorthWest, -1)
	geohashMoveY(&n.NorthWest, 1)
	geohashMoveX(&n.NorthEast, 1)
	geohashMoveY(&n.NorthEast, 1)
	geohashMoveX(&n.SouthEast, 1)
	geohashMoveY(&n.SouthEast, -1)
	geohashMoveX(&n.SouthWest, -1)
	geohashMoveY(&n.SouthWest, -1)
	return n
}

// --- geohash_helper ---

func degToRad(deg float64) float64 { return deg * degreesToRadians }
func radToDeg(rad float64) float64 { return rad / degreesToRadians }

// GeohashEstimateStepsByRadius picks how many splits give boxes about the size
// of a search of rangeMeters around a point at latitude lat: coarse enough that
// the centre box and its eight neighbours cover the search, fine enough not to
// cover much more.
func GeohashEstimateStepsByRadius(rangeMeters, lat float64) uint8 {
	if rangeMeters == 0 {
		return 26
	}
	step := 1
	for rangeMeters < mercatorMax {
		rangeMeters *= 2
		step++
	}
	// One step back, so the range fits inside the box in most cases.
	step -= 2

	// Boxes narrow towards the poles, so take a coarser step there. Measuring
	// the distance between meridians at this latitude would do better than a
	// pair of thresholds, but this does the trick.
	if lat > 66 || lat < -66 {
		step--
		if lat > 80 || lat < -80 {
			step--
		}
	}

	if step < 1 {
		step = 1
	}
	if step > 26 {
		step = 26
	}
	return uint8(step)
}

// GeoShapeType says whether a search area is a circle or a box.
type GeoShapeType uint8

const (
	GeoShapeCircle GeoShapeType = iota + 1
	GeoShapeBox
)

// GeoShape is a search area: a circle of Radius around the centre, or a box of
// Width by Height centred there. Distances are in the caller's unit and
// Conversion turns them into metres.
type GeoShape struct {
	Type GeoShapeType
	// Longitude and Latitude are the centre.
	Longitude, Latitude float64
	// Conversion is metres per unit of Radius, Width and Height.
	Conversion float64
	// Radius applies to a circle.
	Radius float64
	// Width and Height apply to a box.
	Width, Height float64
	// Bounds is filled in by GeohashBoundingBox: the smallest and largest
	// longitude, then the smallest and largest latitude, that the shape
	// reaches.
	Bounds GeoBounds
}

// GeoBounds is the rectangle of degrees a shape fits inside.
type GeoBounds struct {
	MinLong, MinLat, MaxLong, MaxLat float64
}

// GeohashBoundingBox computes the degrees a shape spans and records them in
// shape.Bounds.
//
// A fixed distance east or west covers more degrees the further it is from the
// equator, so a box's edges lean: its wider side is the one nearer a pole. The
// bound is therefore taken at whichever edge is further from the equator.
func GeohashBoundingBox(shape *GeoShape) GeoBounds {
	longitude, latitude := shape.Longitude, shape.Latitude
	var height, width float64
	if shape.Type == GeoShapeCircle {
		height, width = shape.Radius, shape.Radius
	} else {
		height, width = shape.Height/2, shape.Width/2
	}
	height *= shape.Conversion
	width *= shape.Conversion

	latDelta := radToDeg(height / earthRadiusInMeters)
	longDeltaTop := radToDeg(width / earthRadiusInMeters / math.Cos(degToRad(latitude+latDelta)))
	longDeltaBottom := radToDeg(width / earthRadiusInMeters / math.Cos(degToRad(latitude-latDelta)))

	longDelta := longDeltaTop
	if latitude < 0 {
		longDelta = longDeltaBottom
	}
	shape.Bounds = GeoBounds{
		MinLong: longitude - longDelta,
		MaxLong: longitude + longDelta,
		MinLat:  latitude - latDelta,
		MaxLat:  latitude + latDelta,
	}
	return shape.Bounds
}

// GeohashCalculateAreasByShapeWGS84 works out which boxes a search over shape
// has to look in: the box holding the centre plus its neighbours, at a step
// chosen so the nine of them cover the shape, with the neighbours the shape
// does not reach dropped.
func GeohashCalculateAreasByShapeWGS84(shape *GeoShape) GeoHashRadius {
	bounds := GeohashBoundingBox(shape)
	longitude, latitude := shape.Longitude, shape.Latitude

	// For a box the distance that matters is centre to corner.
	radiusMeters := shape.Radius
	if shape.Type == GeoShapeBox {
		radiusMeters = math.Sqrt((shape.Width/2)*(shape.Width/2) + (shape.Height/2)*(shape.Height/2))
	}
	radiusMeters *= shape.Conversion

	steps := GeohashEstimateStepsByRadius(radiusMeters, latitude)

	longRange, latRange := GeohashCoordRange()
	hash, _ := GeohashEncode(longRange, latRange, longitude, latitude, steps)
	neighbors := GeohashNeighbors(hash)
	area, _ := GeohashDecode(longRange, latRange, hash)

	// The estimate can leave a neighbour too small when the centre sits near
	// its box's edge, so that the nine boxes stop short of the search area on
	// one side. If any of the four adjacent boxes does, go a step coarser.
	decreaseStep := false
	{
		north, _ := GeohashDecode(longRange, latRange, neighbors.North)
		south, _ := GeohashDecode(longRange, latRange, neighbors.South)
		east, _ := GeohashDecode(longRange, latRange, neighbors.East)
		west, _ := GeohashDecode(longRange, latRange, neighbors.West)

		if north.Latitude.Max < bounds.MaxLat {
			decreaseStep = true
		}
		if south.Latitude.Min > bounds.MinLat {
			decreaseStep = true
		}
		if east.Longitude.Max < bounds.MaxLong {
			decreaseStep = true
		}
		if west.Longitude.Min > bounds.MinLong {
			decreaseStep = true
		}
	}
	if steps > 1 && decreaseStep {
		steps--
		hash, _ = GeohashEncode(longRange, latRange, longitude, latitude, steps)
		neighbors = GeohashNeighbors(hash)
		area, _ = GeohashDecode(longRange, latRange, hash)
	}

	// Neighbours the search area does not reach into are dropped: a box on
	// the side the area does not extend past contributes nothing.
	if steps >= 2 {
		if area.Latitude.Min < bounds.MinLat {
			neighbors.South = GeoHashBits{}
			neighbors.SouthWest = GeoHashBits{}
			neighbors.SouthEast = GeoHashBits{}
		}
		if area.Latitude.Max > bounds.MaxLat {
			neighbors.North = GeoHashBits{}
			neighbors.NorthEast = GeoHashBits{}
			neighbors.NorthWest = GeoHashBits{}
		}
		if area.Longitude.Min < bounds.MinLong {
			neighbors.West = GeoHashBits{}
			neighbors.SouthWest = GeoHashBits{}
			neighbors.NorthWest = GeoHashBits{}
		}
		if area.Longitude.Max > bounds.MaxLong {
			neighbors.East = GeoHashBits{}
			neighbors.SouthEast = GeoHashBits{}
			neighbors.NorthEast = GeoHashBits{}
		}
	}

	return GeoHashRadius{Hash: hash, Area: area, Neighbors: neighbors}
}

// GeohashAlign52Bits left-aligns a hash of any step into the 52 bits a score
// uses, so hashes at different steps can be compared as scores.
func GeohashAlign52Bits(hash GeoHashBits) uint64 {
	return hash.Bits << (52 - uint(hash.Step)*2)
}

// geohashGetLatDistance is the distance between two latitudes on the same
// meridian. With no longitude difference the haversine formula collapses to the
// angle itself, since asin(sin(x)) is x for latitudes.
func geohashGetLatDistance(lat1, lat2 float64) float64 {
	return earthRadiusInMeters * math.Abs(degToRad(lat2)-degToRad(lat1))
}

// GeohashGetDistance is the great-circle distance in metres between two
// positions, by the haversine formula.
func GeohashGetDistance(lon1, lat1, lon2, lat2 float64) float64 {
	lon1r, lon2r := degToRad(lon1), degToRad(lon2)
	v := math.Sin((lon2r - lon1r) / 2)
	// Same meridian: skip the expensive part.
	if v == 0 {
		return geohashGetLatDistance(lat1, lat2)
	}
	lat1r, lat2r := degToRad(lat1), degToRad(lat2)
	u := math.Sin((lat2r - lat1r) / 2)
	a := u*u + math.Cos(lat1r)*math.Cos(lat2r)*v*v
	return 2.0 * earthRadiusInMeters * math.Asin(math.Sqrt(a))
}

// GeohashGetDistanceIfInRadius returns the distance from (x1, y1) to (x2, y2)
// and whether it is within radius metres.
func GeohashGetDistanceIfInRadius(x1, y1, x2, y2, radius float64) (float64, bool) {
	distance := GeohashGetDistance(x1, y1, x2, y2)
	return distance, distance <= radius
}

// GeohashGetDistanceIfInRectangle reports whether (x2, y2) lies within a box
// of widthM by heightM metres centred on (x1, y1), and its distance from the
// centre if so. Latitude is checked first because it is cheaper.
func GeohashGetDistanceIfInRectangle(widthM, heightM, x1, y1, x2, y2 float64) (float64, bool) {
	if geohashGetLatDistance(y2, y1) > heightM/2 {
		return 0, false
	}
	if GeohashGetDistance(x2, y2, x1, y2) > widthM/2 {
		return 0, false
	}
	return GeohashGetDistance(x1, y1, x2, y2), true
}
