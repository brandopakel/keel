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
	e.recordsItself()
	if len(args) < 4 {
		return e.encode(wrongArguments("GEOADD"), false)
	}
	flags, ch, triples, err := geoaddShape(args)
	if err != nil {
		return e.encode(err, false)
	}
	req := zaddRequest{flags: flags, scores: make([]float64, 0, len(triples)/3),
		members: make([]string, 0, len(triples)/3), text: args, options: args[1 : len(args)-len(triples)]}
	for i := 0; i < len(triples); i += 3 {
		score, err := geoaddScore(triples[i], triples[i+1])
		if err != nil {
			return e.encode(err, false)
		}
		req.scores = append(req.scores, score)
		req.members = append(req.members, triples[i+2])
	}
	res, _ := e.opGeoAdd(args[0], req)
	r := e.reply()
	if ch {
		return r.Int(res.changed).Bytes()
	}
	return r.Int(res.added).Bytes()
}

// geoaddShape reads GEOADD's options - NX, XX and CH, which Redis's
// geoaddCommand takes before the positions - then whether what follows comes
// in threes and NX and XX were not both given, either refused as a syntax
// error, and returns the triples. The positions are read after this.
func geoaddShape(args []string) (flags int, ch bool, triples []string, err error) {
	next := 1
options:
	for next < len(args) {
		switch strings.ToUpper(args[next]) {
		case "NX":
			flags |= data_structure.ZAddNX
		case "XX":
			flags |= data_structure.ZAddXX
		case "CH":
			ch = true
		default:
			break options
		}
		next++
	}
	triples = args[next:]
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
	// A keyspace hit or a miss, counted after the unit was read, as Redis's
	// geodistCommand reads it before it looks the key up.
	e.noteLookup(ok)
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

// geoSearch is a parsed GEOSEARCH, GEOSEARCHSTORE or GEORADIUS.
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
	// storeKey is where STORE, STOREDIST or GEOSEARCHSTORE puts the result;
	// storeDist scores it by distance rather than by geohash.
	storeKey  string
	storeDist bool
}

// geoFlags are the forms of Redis's georadiusGeneric (geo.c, 524).
type geoFlags uint8

const (
	// geoRadiusCoords and geoRadiusMember are GEORADIUS's and
	// GEORADIUSBYMEMBER's centres, given before the options.
	geoRadiusCoords geoFlags = 1 << iota
	geoRadiusMember
	// geoNoStore is the _RO forms', which take no STORE.
	geoNoStore
	// geoSearchForm is GEOSEARCH's, whose centre and extent are options;
	// geoSearchStore its STORE form.
	geoSearchForm
	geoSearchStore
)

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
	zs, exists := e.zsetFor(args[0])
	return e.geoSearchCommand("GEOSEARCH", zs, exists, args[1:], geoSearchForm, &geoSearch{}, nil)
}

// cmdGEOSEARCHSTORE is GEOSEARCH from the second key, stored at the first in
// place of whatever it held: by geohash, or with STOREDIST by distance.
func (e *Engine) cmdGEOSEARCHSTORE(args []string) []byte {
	e.recordsItself()
	if len(args) < 7 {
		return e.encode(wrongArguments("GEOSEARCHSTORE"), false)
	}
	zs, err := e.zsetSource(args[1])
	if err != nil {
		return e.encode(err, false)
	}
	return e.geoSearchCommand("GEOSEARCHSTORE", zs, zs != nil, args[2:], geoSearchForm|geoSearchStore,
		&geoSearch{storeKey: args[0]}, args)
}

// cmdGEORADIUS and its kin take their centre and radius before the options:
// GEORADIUS key longitude latitude radius unit, GEORADIUSBYMEMBER key member
// radius unit; the _RO forms take no STORE.
func (e *Engine) cmdGEORADIUS(args []string) []byte {
	return e.georadius("GEORADIUS", args, geoRadiusCoords)
}
func (e *Engine) cmdGEORADIUSRO(args []string) []byte {
	return e.georadius("GEORADIUS_RO", args, geoRadiusCoords|geoNoStore)
}
func (e *Engine) cmdGEORADIUSBYMEMBER(args []string) []byte {
	return e.georadius("GEORADIUSBYMEMBER", args, geoRadiusMember)
}
func (e *Engine) cmdGEORADIUSBYMEMBERRO(args []string) []byte {
	return e.georadius("GEORADIUSBYMEMBER_RO", args, geoRadiusMember|geoNoStore)
}

func (e *Engine) georadius(name string, args []string, flags geoFlags) []byte {
	e.recordsItself()
	base := 5
	if flags&geoRadiusMember != 0 {
		base = 4
	}
	if len(args) < base {
		return e.encode(wrongArguments(name), false)
	}
	zs, exists := e.zsetFor(args[0])
	s := &geoSearch{centre: "radius", area: "BYRADIUS"}
	s.shape.Type = data_structure.GeoShapeCircle
	if flags&geoRadiusCoords != 0 {
		longitude, latitude, err := parseLongLat(args[1], args[2])
		if err != nil {
			return e.encode(err, false)
		}
		s.shape.Longitude, s.shape.Latitude = longitude, latitude
	} else if exists {
		var ok bool
		if s.shape.Longitude, s.shape.Latitude, ok = geoPosition(zs, args[1]); !ok {
			return e.encode(errGeoMember, false)
		}
	}
	// GEORADIUSBYMEMBER of a missing key reads no radius, as Redis's does not.
	if flags&geoRadiusCoords != 0 || exists {
		radius, err := parseGeoDistance(args[base-2], "ERR need numeric radius", "ERR radius cannot be negative")
		if err != nil {
			return e.encode(err, false)
		}
		toMeters, ok := geoUnitToMeters(args[base-1])
		if !ok {
			return e.encode(errGeoUnit, false)
		}
		s.shape.Radius, s.shape.Conversion = radius, toMeters
	}
	return e.geoSearchCommand(name, zs, exists, args[base:], flags, s, args)
}

var errGeoMember = errors.New("ERR could not decode requested zset member")

// geoSearchCommand reads the options after the centre, then searches and
// answers, or stores. name and text are the command and its arguments, which
// a store records as sent when it changed its destination, as Redis
// propagates it.
func (e *Engine) geoSearchCommand(name string, zs *data_structure.ZSet, exists bool, args []string, flags geoFlags, s *geoSearch, text []string) []byte {
	if err := e.parseGeoSearch(zs, exists, args, flags, s); err != nil {
		return e.encode(err, false)
	}
	if !exists {
		if s.storeKey != "" {
			if e.opZStore(s.storeKey, nil, nil) {
				e.aofRecordArgs(name, text)
			}
			return constant.RespZero
		}
		return constant.RespEmptyArray
	}

	// COUNT without an order still has to mean the nearest ones, so it
	// implies ASC - unless ANY asked for whichever come first.
	if s.count != 0 && s.order == "" && !s.any {
		s.order = "ASC"
	}

	radius := data_structure.GeohashCalculateAreasByShapeWGS84(&s.shape)
	if s.storeKey == "" {
		return e.geoSearchReply(zs, radius, s)
	}
	walk, refusal := e.geoSearchWalk(zs, radius, s)
	if refusal != nil {
		return refusal
	}
	var members []string
	var scores []float64
	walk(func(p data_structure.GeoPoint) bool {
		score := p.Score
		if s.storeDist {
			score = p.Dist / s.shape.Conversion
		}
		members, scores = append(members, p.Member), append(scores, score)
		return true
	})
	if e.opZStore(s.storeKey, members, scores) {
		e.aofRecordArgs(name, text)
	}
	reply := e.reply()
	return reply.Int(int64(len(members))).Bytes()
}

// parseGeoSearch reads the options after the centre, which zs holds if
// exists, as Redis's georadiusGeneric reads them: each keyword where the
// command's form admits it, and anything else a syntax error.
func (e *Engine) parseGeoSearch(zs *data_structure.ZSet, exists bool, args []string, flags geoFlags, s *geoSearch) error {
	search := flags&geoSearchForm != 0
	canStore := !search && flags&geoNoStore == 0
	for i := 0; i < len(args); i++ {
		remaining := len(args) - i - 1
		switch strings.ToUpper(args[i]) {
		case "FROMMEMBER":
			if !search || remaining < 1 || s.centre == "FROMLONLAT" {
				return errSyntax
			}
			s.centre, s.fromMember = "FROMMEMBER", args[i+1]
			if exists {
				var ok bool
				s.shape.Longitude, s.shape.Latitude, ok = geoPosition(zs, s.fromMember)
				if !ok {
					return errGeoMember
				}
			}
			i++
		case "FROMLONLAT":
			if !search || remaining < 2 || s.centre == "FROMMEMBER" {
				return errSyntax
			}
			longitude, latitude, err := parseLongLat(args[i+1], args[i+2])
			if err != nil {
				return err
			}
			s.centre = "FROMLONLAT"
			s.shape.Longitude, s.shape.Latitude = longitude, latitude
			i += 2
		case "BYRADIUS":
			if !search || remaining < 2 || s.area == "BYBOX" {
				return errSyntax
			}
			radius, err := parseGeoDistance(args[i+1], "ERR need numeric radius", "ERR radius cannot be negative")
			if err != nil {
				return err
			}
			toMeters, ok := geoUnitToMeters(args[i+2])
			if !ok {
				return errGeoUnit
			}
			s.area = "BYRADIUS"
			s.shape.Type, s.shape.Radius, s.shape.Conversion = data_structure.GeoShapeCircle, radius, toMeters
			i += 2
		case "BYBOX":
			if !search || remaining < 3 || s.area == "BYRADIUS" {
				return errSyntax
			}
			width, err := parseGeoDistance(args[i+1], "ERR need numeric width", "ERR height or width cannot be negative")
			if err != nil {
				return err
			}
			height, err := parseGeoDistance(args[i+2], "ERR need numeric height", "ERR height or width cannot be negative")
			if err != nil {
				return err
			}
			toMeters, ok := geoUnitToMeters(args[i+3])
			if !ok {
				return errGeoUnit
			}
			s.area = "BYBOX"
			s.shape.Type, s.shape.Width, s.shape.Height, s.shape.Conversion = data_structure.GeoShapeBox, width, height, toMeters
			i += 3
		case "ASC", "DESC":
			s.order = strings.ToUpper(args[i])
		case "COUNT":
			if remaining < 1 {
				return errSyntax
			}
			n, valid := e.counterInteger(args[i+1])
			if !valid {
				return errNotAnInteger
			}
			if n <= 0 {
				return errors.New("ERR COUNT must be > 0")
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
		case "STORE", "STOREDIST":
			dist := strings.EqualFold(args[i], "STOREDIST")
			switch {
			case canStore && remaining >= 1:
				s.storeKey, s.storeDist = args[i+1], dist
				i++
			case dist && flags&geoSearchStore != 0:
				s.storeDist = true
			default:
				return errSyntax
			}
		default:
			return errSyntax
		}
	}
	if s.storeKey != "" && (s.withDist || s.withHash || s.withCoord) {
		if flags&geoSearchStore != 0 {
			return errors.New("ERR GEOSEARCHSTORE is not compatible with WITHDIST, WITHHASH and WITHCOORD options")
		}
		return errors.New("ERR STORE option in GEORADIUS is not compatible with WITHDIST, WITHHASH and WITHCOORD options")
	}
	// Redis names the command as it was sent in these two.
	if s.centre == "" {
		return fmt.Errorf("ERR exactly one of FROMMEMBER or FROMLONLAT can be specified for %s", EchoArgument(e.runningName))
	}
	if s.area == "" {
		return fmt.Errorf("ERR exactly one of BYRADIUS and BYBOX can be specified for %s", EchoArgument(e.runningName))
	}
	if s.any && s.count == 0 {
		return errors.New("ERR the ANY argument requires COUNT argument")
	}
	return nil
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
