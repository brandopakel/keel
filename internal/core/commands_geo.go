package core

import (
	"errors"
	"fmt"
	"math"
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
	// NaN is refused by name: it parses, and every range comparison below is
	// false for it, so it would otherwise walk straight through.
	longitude, err = strconv.ParseFloat(longS, 64)
	if err != nil || math.IsNaN(longitude) {
		return 0, 0, errNotAFloat
	}
	latitude, err = strconv.ParseFloat(latS, 64)
	if err != nil || math.IsNaN(latitude) {
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
func (e *Engine) cmdGEOADD(args []string) []byte {
	if len(args) < 4 {
		return e.encode(wrongArguments("GEOADD"), false)
	}
	flags, ch, triples, err := geoaddShape(args)
	if err != nil {
		return e.encode(err, false)
	}
	scores := make([]float64, 0, len(triples)/3)
	members := make([]string, 0, len(triples)/3)
	for i := 0; i < len(triples); i += 3 {
		score, err := geoaddScore(triples[i], triples[i+1])
		if err != nil {
			return e.encode(err, false)
		}
		scores = append(scores, score)
		members = append(members, triples[i+2])
	}
	added, changed := e.zaddApply(args[0], scores, members, flags)
	if ch {
		return e.encode(changed, false)
	}
	return e.encode(added, false)
}

// geoaddShape reads GEOADD's options in Redis's order, then whether what
// follows comes in threes and NX and XX were not both given, either refused as
// a syntax error, and returns the triples. The positions are read after this.
func geoaddShape(args []string) (flags int, ch bool, triples []string, err error) {
	flags, ch, next := zaddOptions(args[1:])
	triples = args[1+next:]
	if len(triples) == 0 || len(triples)%3 != 0 ||
		flags&data_structure.ZAddNX != 0 && flags&data_structure.ZAddXX != 0 {
		return 0, false, nil, errSyntax
	}
	return flags, ch, triples, nil
}

// geoaddScore reads one position as the score that indexes it.
func geoaddScore(longS, latS string) (float64, error) {
	longitude, latitude, err := parseLongLat(longS, latS)
	if err != nil {
		return 0, err
	}
	score, ok := data_structure.GeoScore(longitude, latitude)
	if !ok {
		return 0, fmt.Errorf("ERR invalid longitude,latitude pair %f,%f", longitude, latitude)
	}
	return float64(score), nil
}

// geoaddArguments is GEOADD's refusal of its arguments, or nil: its shape,
// then every position, as Redis reads them before it looks at the key.
func geoaddArguments(args []string) error {
	_, _, triples, err := geoaddShape(args)
	for i := 0; err == nil && i < len(triples); i += 3 {
		_, err = geoaddScore(triples[i], triples[i+1])
	}
	return err
}

// geodistUnit reads GEODIST's optional unit, refused as Redis refuses it
// before the key is looked at; anything after it is a syntax error.
func geodistUnit(args []string) (float64, error) {
	if len(args) > 4 {
		return 0, errSyntax
	}
	if len(args) < 4 {
		return 1, nil
	}
	toMeters, ok := geoUnitToMeters(args[3])
	if !ok {
		return 0, errGeoUnit
	}
	return toMeters, nil
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
// the worst case. A missing key or member answers nil. The distance is a bulk
// string in RESP3 as well as RESP2: Redis keeps it one, at four decimals,
// rather than sending a double.
func (e *Engine) cmdGEODIST(args []string) []byte {
	if len(args) < 3 {
		return e.encode(wrongArguments("GEODIST"), false)
	}
	toMeters, err := geodistUnit(args)
	if err != nil {
		return e.encode(err, false)
	}
	zs, ok := e.zsetFor(args[0])
	if !ok {
		return e.nullReply()
	}
	long1, lat1, ok1 := geoPosition(zs, args[1])
	long2, lat2, ok2 := geoPosition(zs, args[2])
	if !ok1 || !ok2 {
		return e.nullReply()
	}
	return e.encode(formatDistance(data_structure.GeohashGetDistance(long1, lat1, long2, lat2)/toMeters), false)
}

// cmdGEOHASH implements GEOHASH key [member ...]: one standard eleven-character
// geohash per member, in the order asked, with nil standing in for a member
// that is not there.
func (e *Engine) cmdGEOHASH(args []string) []byte {
	if len(args) < 1 {
		return e.encode(wrongArguments("GEOHASH"), false)
	}
	zs, exists := e.zsetFor(args[0])
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
	return e.encode(out, false)
}

// cmdGEOPOS implements GEOPOS key [member ...]: a longitude, latitude pair per
// member, in the order asked, with a null array for a member that is not there.
// The coordinates are doubles in RESP3 and bulk strings in RESP2.
func (e *Engine) cmdGEOPOS(args []string) []byte {
	if len(args) < 1 {
		return e.encode(wrongArguments("GEOPOS"), false)
	}
	zs, exists := e.zsetFor(args[0])
	out := appendArrayHeader(nil, len(args)-1)
	for _, member := range args[1:] {
		if !exists {
			out = e.appendNullArray(out)
			continue
		}
		longitude, latitude, ok := geoPosition(zs, member)
		if !ok {
			out = e.appendNullArray(out)
			continue
		}
		out = appendArrayHeader(out, 2)
		out = appendDouble(e.framing, out, formatCoordinate(longitude))
		out = appendDouble(e.framing, out, formatCoordinate(latitude))
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
// The arguments are read as Redis reads them, in order, so the first one Redis
// would refuse is the one refused here, and in its words: FROMMEMBER is
// resolved as it is read, and a centre or an extent may be given again, the
// last one counting, but not alongside the other kind.
//
// Without WITH options the reply is an array of members. With any of them each
// member becomes an array of the member followed by, in this order, its
// distance in the search's unit, its raw geohash score and its coordinates,
// whichever were asked for.
func (e *Engine) cmdGEOSEARCH(args []string) []byte {
	if len(args) < 6 {
		return e.encode(wrongArguments("GEOSEARCH"), false)
	}
	key := args[0]
	zs, exists := e.zsetFor(key)

	s, err := e.parseGeoSearch(zs, exists, args[1:])
	if err != nil {
		return e.encode(err, false)
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
	return e.geoSearchReply(zs, radius, s)
}

// parseGeoSearch reads everything after the key, which zs holds if exists.
func (e *Engine) parseGeoSearch(zs *data_structure.ZSet, exists bool, args []string) (*geoSearch, error) {
	s := &geoSearch{}
	for i := 0; i < len(args); i++ {
		remaining := len(args) - i - 1
		switch strings.ToUpper(args[i]) {
		case "FROMMEMBER":
			if remaining < 1 || s.centre == "FROMLONLAT" {
				return nil, errSyntax
			}
			s.centre, s.fromMember = "FROMMEMBER", args[i+1]
			if exists {
				var ok bool
				s.shape.Longitude, s.shape.Latitude, ok = geoPosition(zs, s.fromMember)
				if !ok {
					return nil, errors.New("ERR could not decode requested zset member")
				}
			}
			i++
		case "FROMLONLAT":
			if remaining < 2 || s.centre == "FROMMEMBER" {
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
			if remaining < 2 || s.area == "BYBOX" {
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
			if remaining < 3 || s.area == "BYRADIUS" {
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
			n, valid := e.counterInteger(args[i+1])
			if !valid {
				return nil, errNotAnInteger
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
			return nil, errSyntax
		}
	}
	// Redis names the command as it was sent in these two.
	if s.centre == "" {
		return nil, fmt.Errorf("ERR exactly one of FROMMEMBER or FROMLONLAT can be specified for %s", EchoArgument(e.runningName))
	}
	if s.area == "" {
		return nil, fmt.Errorf("ERR exactly one of BYRADIUS and BYBOX can be specified for %s", EchoArgument(e.runningName))
	}
	if s.any && s.count == 0 {
		return nil, errors.New("ERR the ANY argument requires COUNT argument")
	}
	return s, nil
}

// parseGeoDistance reads a radius, width or height.
func parseGeoDistance(s, notNumeric, negative string) (float64, error) {
	d, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(d) {
		return 0, errors.New(notNumeric)
	}
	if d < 0 {
		return 0, errors.New(negative)
	}
	return d, nil
}
