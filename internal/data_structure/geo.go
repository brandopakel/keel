/*
 * Copyright (c) 2014, Matt Stancliff <matt@genges.com>.
 * Copyright (c) 2015-2016, Salvatore Sanfilippo <antirez@gmail.com>.
 * All rights reserved.
 *
 * Redistribution and use in source and binary forms, with or without
 * modification, are permitted provided that the following conditions are met:
 *
 *   * Redistributions of source code must retain the above copyright notice,
 *     this list of conditions and the following disclaimer.
 *   * Redistributions in binary form must reproduce the above copyright
 *     notice, this list of conditions and the following disclaimer in the
 *     documentation and/or other materials provided with the distribution.
 *   * Neither the name of Redis nor the names of its contributors may be used
 *     to endorse or promote products derived from this software without
 *     specific prior written permission.
 *
 * THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
 * AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
 * IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE
 * ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT OWNER OR CONTRIBUTORS BE
 * LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR
 * CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF
 * SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS
 * INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN
 * CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE)
 * ARISING IN ANY WAY OUT OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE
 * POSSIBILITY OF SUCH DAMAGE.
 */

// This file is a Go translation of the search and hashing helpers in
// src/geo.c from Redis 7.2.4, which carries the licence above. The translation
// was made for keel by Brando Pakel in 2026 and is distributed under the same
// terms.

package data_structure

// Geospatial indexing over a sorted set.
//
// A position is stored as a member of a sorted set whose score is its 52-bit
// geohash, aligned so that a coarser hash is a prefix of a finer one. Every
// position inside a box of the map therefore has a score inside one interval,
// and a radius search is a range query per box over the sorted set, followed
// by an exact distance check on each candidate.

// GeoPoint is one member found by a search, with where it is and how far it
// was from the centre.
type GeoPoint struct {
	Longitude, Latitude float64
	// Dist is the distance from the search centre, in metres.
	Dist float64
	// Score is the member's geohash as stored.
	Score  float64
	Member string
}

// GeoScore is the score a position is stored under: its hash at the finest
// step, aligned to 52 bits. It reports false for a position outside the
// supported range.
func GeoScore(longitude, latitude float64) (uint64, bool) {
	hash, ok := GeohashEncodeWGS84(longitude, latitude, GeoStepMax)
	if !ok {
		return 0, false
	}
	return GeohashAlign52Bits(hash), true
}

// GeoDecodeScore is the position a stored score stands for.
func GeoDecodeScore(score float64) (longitude, latitude float64, ok bool) {
	hash := GeoHashBits{Bits: uint64(score), Step: GeoStepMax}
	return GeohashDecodeToLongLatWGS84(hash)
}

// geoWithinShape decodes a score and reports whether its position falls inside
// the shape, with the position and its distance from the centre if so.
func geoWithinShape(shape *GeoShape, score float64) (longitude, latitude, distance float64, ok bool) {
	longitude, latitude, ok = GeoDecodeScore(score)
	if !ok {
		return 0, 0, 0, false
	}
	switch shape.Type {
	case GeoShapeCircle:
		distance, ok = GeohashGetDistanceIfInRadius(shape.Longitude, shape.Latitude,
			longitude, latitude, shape.Radius*shape.Conversion)
	case GeoShapeBox:
		distance, ok = GeohashGetDistanceIfInRectangle(shape.Width*shape.Conversion,
			shape.Height*shape.Conversion, shape.Longitude, shape.Latitude, longitude, latitude)
	default:
		ok = false
	}
	return longitude, latitude, distance, ok
}

// geoPointsInRange appends to out every member whose score is in [min, max)
// and whose position is inside the shape. When limit is positive it stops once
// out holds that many points.
//
// Appending to a slice the caller keeps across boxes is what makes a search a
// handful of range walks rather than a merge, and rejecting points outside the
// shape here means only the points actually returned are ever collected.
func (zs *ZSet) geoPointsInRange(min, max float64, shape *GeoShape, out []GeoPoint, limit int) []GeoPoint {
	r := rangeSpec{min: min, max: max, maxEx: true}
	for n := zs.sl.firstInRange(r); n != nil && r.lteMax(n.score); n = n.next() {
		if longitude, latitude, distance, ok := geoWithinShape(shape, n.score); ok {
			out = append(out, GeoPoint{
				Longitude: longitude,
				Latitude:  latitude,
				Dist:      distance,
				Score:     n.score,
				Member:    n.ele,
			})
		}
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// scoresOfGeoHashBox is the interval of scores that lie inside a box: from the
// hash aligned to 52 bits, up to but not including the next hash at the same
// step aligned the same way. Every finer hash inside the box shares the box's
// bits as a prefix, so it falls between the two.
func scoresOfGeoHashBox(hash GeoHashBits) (min, max uint64) {
	min = GeohashAlign52Bits(hash)
	hash.Bits++
	max = GeohashAlign52Bits(hash)
	return min, max
}

// GeoMembersOfAllNeighbors collects every member inside the shape from the
// nine boxes a search has to look in. When limit is positive it stops once
// that many have been found.
func (zs *ZSet) GeoMembersOfAllNeighbors(radius GeoHashRadius, shape *GeoShape, limit int) []GeoPoint {
	n := radius.Neighbors
	boxes := [9]GeoHashBits{
		radius.Hash,
		n.North, n.South, n.East, n.West,
		n.NorthEast, n.NorthWest, n.SouthEast, n.SouthWest,
	}

	var out []GeoPoint
	lastProcessed := -1
	for i, box := range boxes {
		// A zero box was dropped because the shape does not reach it.
		if box.IsZero() {
			continue
		}
		// At a very large radius adjacent boxes can be the same box, and
		// walking it twice would return its members twice.
		if lastProcessed >= 0 && box == boxes[lastProcessed] {
			continue
		}
		if limit > 0 && len(out) >= limit {
			break
		}
		min, max := scoresOfGeoHashBox(box)
		out = zs.geoPointsInRange(float64(min), float64(max), shape, out, limit)
		lastProcessed = i
	}
	return out
}

// geoAlphabet is the base32 alphabet of the geohash standard.
const geoAlphabet = "0123456789bcdefghjkmnpqrstuvwxyz"

// GeohashString is the eleven-character geohash of a position, as the rest of
// the world writes them.
//
// Scores are encoded against a latitude range of -85 to 85, which is not what
// a standard geohash uses, so the position is re-encoded against -90 to 90
// before being spelled out. Fifty-two bits are ten characters and a fraction;
// the eleventh is written as zero.
func GeohashString(longitude, latitude float64) (string, bool) {
	standardLong := GeoHashRange{Min: -180, Max: 180}
	standardLat := GeoHashRange{Min: -90, Max: 90}
	hash, ok := GeohashEncode(standardLong, standardLat, longitude, latitude, GeoStepMax)
	if !ok {
		return "", false
	}

	var buf [11]byte
	for i := 0; i < 11; i++ {
		idx := 0
		if i < 10 {
			idx = int(hash.Bits>>(52-uint(i+1)*5)) & 0x1f
		}
		buf[i] = geoAlphabet[idx]
	}
	return string(buf[:]), true
}
