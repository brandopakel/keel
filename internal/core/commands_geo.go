package core

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/brandopakel/keel/internal/constant"
	"github.com/brandopakel/keel/internal/data_structure"
)

// Geospatial commands.
//
// A geo key is a sorted set whose scores are geohashes, exactly as in Redis, so
// GEOADD is a ZADD with the coordinates turned into a score, ZCARD counts the
// positions, and the type check treats the two as one keyspace. What the geo
// commands add is the arithmetic: turning coordinates into scores and back,
// distances, and the range queries a radius search becomes.

// geoUnitToMeters is how many metres one of the named units is.
func geoUnitToMeters(unit string) (float64, bool) {
	switch strings.ToLower(unit) {
	case "m":
		return 1, true
	case "km":
		return 1000, true
	case "ft":
		return 0.3048, true
	case "mi":
		return 1609.34, true
	}
	return 0, false
}

var errGeoUnit = errors.New("ERR unsupported unit provided. please use M, KM, FT, MI")

// parseLongLat reads a longitude and latitude, refusing a pair the index
// cannot hold. The error carries the pair, as Redis's does, because a swapped
// latitude and longitude is the usual way to arrive here.
func parseLongLat(longS, latS string) (longitude, latitude float64, err error) {
	longitude, err = strconv.ParseFloat(longS, 64)
	if err != nil {
		return 0, 0, errNotAFloat
	}
	latitude, err = strconv.ParseFloat(latS, 64)
	if err != nil {
		return 0, 0, errNotAFloat
	}
	if longitude < data_structure.GeoLongMin || longitude > data_structure.GeoLongMax ||
		latitude < data_structure.GeoLatMin || latitude > data_structure.GeoLatMax {
		return 0, 0, fmt.Errorf("ERR invalid longitude,latitude pair %f,%f", longitude, latitude)
	}
	return longitude, latitude, nil
}

// formatCoordinate writes a coordinate with the fewest digits that read back
// to the same float64.
func formatCoordinate(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// formatDistance writes a distance to four decimals: enough to be accurate
// when the unit is the kilometre, without the noise of a full float64.
func formatDistance(v float64) string { return strconv.FormatFloat(v, 'f', 4, 64) }

// cmdGEOADD implements GEOADD key [NX|XX] [CH] longitude latitude member [...].
//
// Every position is checked before any is stored, so one bad pair in a batch
// leaves the key as it was.
func cmdGEOADD(args []string) []byte {
	if len(args) < 4 {
		return Encode(errors.New("ERR wrong number of arguments for 'GEOADD' command"), false)
	}
	key := args[0]
	flags, ch, next := zaddOptions(args[1:])
	if flags&data_structure.ZAddNX != 0 && flags&data_structure.ZAddXX != 0 {
		return Encode(errNXWithXX, false)
	}
	triples := args[1+next:]
	if len(triples) == 0 || len(triples)%3 != 0 {
		return Encode(errSyntax, false)
	}

	scores := make([]float64, 0, len(triples)/3)
	members := make([]string, 0, len(triples)/3)
	for i := 0; i < len(triples); i += 3 {
		longitude, latitude, err := parseLongLat(triples[i], triples[i+1])
		if err != nil {
			return Encode(err, false)
		}
		score, ok := data_structure.GeoScore(longitude, latitude)
		if !ok {
			return Encode(fmt.Errorf("ERR invalid longitude,latitude pair %f,%f", longitude, latitude), false)
		}
		scores = append(scores, float64(score))
		members = append(members, triples[i+2])
	}

	added, changed := zaddApply(key, scores, members, flags)
	if ch {
		return Encode(changed, false)
	}
	return Encode(added, false)
}

// geoPosition is where a member of a geo key is, if it has one.
func geoPosition(zs *data_structure.ZSet, member string) (longitude, latitude float64, ok bool) {
	score, ok := zs.Score(member)
	if !ok {
		return 0, 0, false
	}
	return data_structure.GeoDecodeScore(score)
}

// cmdGEODIST implements GEODIST key member1 member2 [M|KM|FT|MI].
//
// The distance assumes a spherical Earth, so it can be off by up to 0.5% in
// the worst case. A missing key or member answers nil.
func cmdGEODIST(args []string) []byte {
	if len(args) != 3 && len(args) != 4 {
		return Encode(errors.New("ERR wrong number of arguments for 'GEODIST' command"), false)
	}
	toMeters := 1.0
	if len(args) == 4 {
		var ok bool
		if toMeters, ok = geoUnitToMeters(args[3]); !ok {
			return Encode(errGeoUnit, false)
		}
	}
	zs, ok := zsetFor(args[0])
	if !ok {
		return constant.RespNil
	}
	long1, lat1, ok1 := geoPosition(zs, args[1])
	long2, lat2, ok2 := geoPosition(zs, args[2])
	if !ok1 || !ok2 {
		return constant.RespNil
	}
	return Encode(formatDistance(data_structure.GeohashGetDistance(long1, lat1, long2, lat2)/toMeters), false)
}

// cmdGEOHASH implements GEOHASH key [member ...]: one standard eleven-character
// geohash per member, in the order asked, with nil standing in for a member
// that is not there.
func cmdGEOHASH(args []string) []byte {
	if len(args) < 1 {
		return Encode(errors.New("ERR wrong number of arguments for 'GEOHASH' command"), false)
	}
	zs, exists := zsetFor(args[0])
	out := make([]interface{}, 0, len(args)-1)
	for _, member := range args[1:] {
		if !exists {
			out = append(out, nil)
			continue
		}
		longitude, latitude, ok := geoPosition(zs, member)
		if !ok {
			out = append(out, nil)
			continue
		}
		hash, ok := data_structure.GeohashString(longitude, latitude)
		if !ok {
			out = append(out, nil)
			continue
		}
		out = append(out, hash)
	}
	return Encode(out, false)
}

// cmdGEOPOS implements GEOPOS key [member ...]: a longitude, latitude pair per
// member, in the order asked, with a null array for a member that is not there.
func cmdGEOPOS(args []string) []byte {
	if len(args) < 1 {
		return Encode(errors.New("ERR wrong number of arguments for 'GEOPOS' command"), false)
	}
	zs, exists := zsetFor(args[0])
	out := appendArrayHeader(nil, len(args)-1)
	for _, member := range args[1:] {
		if !exists {
			out = append(out, constant.RespNilArray...)
			continue
		}
		longitude, latitude, ok := geoPosition(zs, member)
		if !ok {
			out = append(out, constant.RespNilArray...)
			continue
		}
		out = append(out, Encode([]string{formatCoordinate(longitude), formatCoordinate(latitude)}, false)...)
	}
	return out
}

// geoSearch is a parsed GEOSEARCH.
type geoSearch struct {
	shape data_structure.GeoShape
	// centre says how the centre was given; area how the extent was.
	centre, area string
	// fromMember is the member named by FROMMEMBER, resolved once the key is
	// known to exist.
	fromMember string

	withDist, withHash, withCoord bool
	// order is "ASC", "DESC" or "" for the order found.
	order string
	// count is the most results to return; zero is all of them. any stops the
	// search as soon as count have been found rather than finding all and
	// keeping the nearest.
	count int
	any   bool
}

// cmdGEOSEARCH implements
//
//	GEOSEARCH key <FROMMEMBER member | FROMLONLAT longitude latitude>
//	              <BYRADIUS radius M|KM|FT|MI | BYBOX width height M|KM|FT|MI>
//	              [ASC|DESC] [COUNT count [ANY]] [WITHCOORD] [WITHDIST] [WITHHASH]
//
// A bare number in the last position is also accepted as a radius in metres,
// which is how this server's GEOSEARCH used to be called before it took the
// Redis form; a log written then still has to replay.
//
// Without WITH options the reply is an array of members. With any of them each
// member becomes an array of the member followed by, in this order, its
// distance in the search's unit, its raw geohash score and its coordinates,
// whichever were asked for.
func cmdGEOSEARCH(args []string) []byte {
	if len(args) < 2 {
		return Encode(errors.New("ERR wrong number of arguments for 'GEOSEARCH' command"), false)
	}
	key := args[0]
	zs, exists := zsetFor(key)

	s, err := parseGeoSearch(args[1:])
	if err != nil {
		return Encode(err, false)
	}
	if s.centre == "FROMMEMBER" && exists {
		var ok bool
		s.shape.Longitude, s.shape.Latitude, ok = geoPosition(zs, s.fromMember)
		if !ok {
			return Encode(errors.New("ERR could not decode requested zset member"), false)
		}
	}
	if !exists {
		return constant.RespEmptyArray
	}

	// COUNT without an order still has to mean the nearest ones, so it
	// implies ASC - unless ANY asked for whichever come first.
	if s.count != 0 && s.order == "" && !s.any {
		s.order = "ASC"
	}

	radius := data_structure.GeohashCalculateAreasByShapeWGS84(&s.shape)
	limit := 0
	if s.any {
		limit = s.count
	}
	points := zs.GeoMembersOfAllNeighbors(radius, &s.shape, limit)
	if len(points) == 0 {
		return constant.RespEmptyArray
	}

	switch s.order {
	case "ASC":
		sort.Slice(points, func(i, j int) bool { return points[i].Dist < points[j].Dist })
	case "DESC":
		sort.Slice(points, func(i, j int) bool { return points[i].Dist > points[j].Dist })
	}
	if s.count > 0 && len(points) > s.count {
		points = points[:s.count]
	}

	if !s.withDist && !s.withHash && !s.withCoord {
		members := make([]string, len(points))
		for i, p := range points {
			members[i] = p.Member
		}
		return Encode(members, false)
	}

	out := make([]interface{}, 0, len(points))
	for _, p := range points {
		entry := []interface{}{p.Member}
		if s.withDist {
			entry = append(entry, formatDistance(p.Dist/s.shape.Conversion))
		}
		if s.withHash {
			entry = append(entry, int64(p.Score))
		}
		if s.withCoord {
			entry = append(entry, []string{formatCoordinate(p.Longitude), formatCoordinate(p.Latitude)})
		}
		out = append(out, entry)
	}
	return Encode(out, false)
}

// parseGeoSearch reads everything after the key.
func parseGeoSearch(args []string) (*geoSearch, error) {
	s := &geoSearch{}
	for i := 0; i < len(args); i++ {
		remaining := len(args) - i - 1
		switch strings.ToUpper(args[i]) {
		case "FROMMEMBER":
			if remaining < 1 || s.centre != "" {
				return nil, errSyntax
			}
			s.centre, s.fromMember = "FROMMEMBER", args[i+1]
			i++
		case "FROMLONLAT":
			if remaining < 2 || s.centre != "" {
				return nil, errSyntax
			}
			longitude, latitude, err := parseLongLat(args[i+1], args[i+2])
			if err != nil {
				return nil, err
			}
			s.centre = "FROMLONLAT"
			s.shape.Longitude, s.shape.Latitude = longitude, latitude
			i += 2
		case "BYRADIUS":
			if remaining < 2 || s.area != "" {
				return nil, errSyntax
			}
			radius, err := parseGeoDistance(args[i+1], "ERR need numeric radius", "ERR radius cannot be negative")
			if err != nil {
				return nil, err
			}
			toMeters, ok := geoUnitToMeters(args[i+2])
			if !ok {
				return nil, errGeoUnit
			}
			s.area = "BYRADIUS"
			s.shape.Type, s.shape.Radius, s.shape.Conversion = data_structure.GeoShapeCircle, radius, toMeters
			i += 2
		case "BYBOX":
			if remaining < 3 || s.area != "" {
				return nil, errSyntax
			}
			width, err := parseGeoDistance(args[i+1], "ERR need numeric width", "ERR height or width cannot be negative")
			if err != nil {
				return nil, err
			}
			height, err := parseGeoDistance(args[i+2], "ERR need numeric height", "ERR height or width cannot be negative")
			if err != nil {
				return nil, err
			}
			toMeters, ok := geoUnitToMeters(args[i+3])
			if !ok {
				return nil, errGeoUnit
			}
			s.area = "BYBOX"
			s.shape.Type, s.shape.Width, s.shape.Height, s.shape.Conversion = data_structure.GeoShapeBox, width, height, toMeters
			i += 3
		case "ASC", "DESC":
			s.order = strings.ToUpper(args[i])
		case "COUNT":
			if remaining < 1 {
				return nil, errSyntax
			}
			n, err := strconv.ParseInt(args[i+1], 10, 64)
			if err != nil {
				return nil, errors.New("ERR value is not an integer or out of range")
			}
			if n <= 0 {
				return nil, errors.New("ERR COUNT must be > 0")
			}
			s.count = int(n)
			i++
		case "ANY":
			s.any = true
		case "WITHCOORD":
			s.withCoord = true
		case "WITHDIST":
			s.withDist = true
		case "WITHHASH":
			s.withHash = true
		default:
			// The old form: a radius in metres and nothing after it.
			if remaining == 0 && s.area == "" {
				if radius, err := strconv.ParseFloat(args[i], 64); err == nil && radius >= 0 {
					s.area = "BYRADIUS"
					s.shape.Type, s.shape.Radius, s.shape.Conversion = data_structure.GeoShapeCircle, radius, 1
					continue
				}
			}
			return nil, errSyntax
		}
	}
	if s.centre == "" {
		return nil, errors.New("ERR exactly one of FROMMEMBER or FROMLONLAT can be specified for GEOSEARCH")
	}
	if s.area == "" {
		return nil, errors.New("ERR exactly one of BYRADIUS and BYBOX can be specified for GEOSEARCH")
	}
	if s.any && s.count == 0 {
		return nil, errors.New("ERR the ANY argument requires COUNT argument")
	}
	return s, nil
}

// parseGeoDistance reads a radius, width or height.
func parseGeoDistance(s, notNumeric, negative string) (float64, error) {
	d, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, errors.New(notNumeric)
	}
	if d < 0 {
		return 0, errors.New(negative)
	}
	return d, nil
}
