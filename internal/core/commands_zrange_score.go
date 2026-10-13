package core

import (
	"errors"
	"strings"

	"github.com/brandopakel/keel/internal/constant"
	"github.com/brandopakel/keel/internal/data_structure"
)

func scoreBound(text string) (value float64, exclusive bool, err error) {
	if strings.HasPrefix(text, "(") {
		exclusive = true
		text = text[1:]
	}
	value, err = parseZScore(text)
	return
}

// scoreInterval is a range of scores, each end open or closed.
type scoreInterval struct {
	min, max     float64
	minEx, maxEx bool
}

var errMinMaxNotFloat = errors.New("ERR min or max is not a float")

// parseScoreInterval reads a range of scores lowest first, refused in Redis's
// words for either end.
func parseScoreInterval(lower, upper string) (r scoreInterval, err error) {
	var e1, e2 error
	r.min, r.minEx, e1 = scoreBound(lower)
	r.max, r.maxEx, e2 = scoreBound(upper)
	if e1 != nil || e2 != nil {
		return r, errMinMaxNotFloat
	}
	return r, nil
}

func (e *Engine) cmdZCOUNT(args []string) []byte {
	if len(args) != 3 {
		return e.encode(wrongArguments("ZCOUNT"), false)
	}
	r, err := parseScoreInterval(args[1], args[2])
	if err != nil {
		return e.encode(err, false)
	}
	z, ok := e.zsetFor(args[0])
	// A keyspace hit or a miss, counted after the range was read, as Redis's
	// zcountCommand reads it before it looks the key up.
	e.noteLookup(ok)
	reply := e.reply()
	if !ok {
		return reply.Int(0).Bytes()
	}
	return reply.Int(int64(z.CountByScore(r.min, r.max, r.minEx, r.maxEx))).Bytes()
}

// zrangeKind is how a range is given: by rank, by score, or by member.
type zrangeKind uint8

const (
	zrangeAuto zrangeKind = iota
	zrangeRank
	zrangeScore
	zrangeLex
)

// zrangeDirection is a range's direction: chosen by REV (auto), or fixed by
// the command's name.
type zrangeDirection uint8

const (
	zrangeDirectionAuto zrangeDirection = iota
	zrangeForward
	zrangeReverse
)

// zrangeArguments is how a range was asked for.
type zrangeArguments struct {
	kind                zrangeKind
	reverse, withScores bool
	// offset and count are LIMIT's, count -1 when it was not given.
	offset, count int
	start, stop   int
	scores        scoreInterval
	lex           data_structure.LexRange
}

var (
	errLexRange        = errors.New("ERR min or max not valid string range item")
	errLimitWithRank   = errors.New("ERR syntax error, LIMIT is only supported in combination with either BYSCORE or BYLEX")
	errWithScoresBylex = errors.New("ERR syntax error, WITHSCORES not supported in combination with BYLEX")
)

// parseLexInterval reads a range of members lowest first, refused in Redis's
// words for either end.
func parseLexInterval(lower, upper string) (data_structure.LexRange, error) {
	min, ok1 := data_structure.ParseLexBound(lower)
	max, ok2 := data_structure.ParseLexBound(upper)
	if !ok1 || !ok2 {
		return data_structure.LexRange{}, errLexRange
	}
	return data_structure.LexRange{Min: min, Max: max}, nil
}

// parseZRangeArguments reads key min max [options] the way Redis's
// zrangeGenericCommand does for every range command (t_zset.c, 3930-4021):
// the options first - WITHSCORES (not for a store), LIMIT offset count, and
// REV, BYSCORE and BYLEX where the command's name leaves them open - then
// whether they agree, then the range itself, given from the first end walked.
func (e *Engine) parseZRangeArguments(args []string, store bool, kind zrangeKind, dir zrangeDirection) (z zrangeArguments, err error) {
	z.count = -1
	for i := 3; i < len(args); i++ {
		switch opt := strings.ToUpper(args[i]); {
		case !store && opt == "WITHSCORES":
			z.withScores = true
		case opt == "LIMIT" && i+2 < len(args):
			if z.offset, z.count, err = e.integerRange(args[i+1], args[i+2]); err != nil {
				return z, err
			}
			i += 2
		case dir == zrangeDirectionAuto && opt == "REV":
			dir = zrangeReverse
		case kind == zrangeAuto && opt == "BYLEX":
			kind = zrangeLex
		case kind == zrangeAuto && opt == "BYSCORE":
			kind = zrangeScore
		default:
			return z, errSyntax
		}
	}
	if kind == zrangeAuto {
		kind = zrangeRank
	}
	z.kind, z.reverse = kind, dir == zrangeReverse
	if z.count != -1 && kind == zrangeRank {
		return z, errLimitWithRank
	}
	if z.withScores && kind == zrangeLex {
		return z, errWithScoresBylex
	}
	lower, upper := args[1], args[2]
	if z.reverse && kind != zrangeRank {
		lower, upper = upper, lower
	}
	switch kind {
	case zrangeScore:
		z.scores, err = parseScoreInterval(lower, upper)
	case zrangeLex:
		z.lex, err = parseLexInterval(lower, upper)
	default:
		z.start, z.stop, err = e.integerRange(lower, upper)
	}
	return z, err
}

// zrangeWalk walks z's range of zs, member by member with its score.
func zrangeWalk(zs *data_structure.ZSet, z zrangeArguments) func(func(string, float64) bool) {
	return func(yield func(string, float64) bool) {
		switch z.kind {
		case zrangeScore:
			r := z.scores
			zs.VisitRangeByScore(r.min, r.max, r.minEx, r.maxEx, z.offset, z.count, z.reverse, yield)
		case zrangeLex:
			zs.VisitRangeByLex(z.lex, z.offset, z.count, z.reverse, yield)
		default:
			zs.VisitRangeByRank(z.start, z.stop, z.reverse, yield)
		}
	}
}

// zrangeCommand answers a range command: ZRANGE, ZRANGEBYSCORE,
// ZREVRANGEBYSCORE, ZREVRANGE, ZRANGEBYLEX and ZREVRANGEBYLEX.
func (e *Engine) zrangeCommand(name string, args []string, kind zrangeKind, dir zrangeDirection) []byte {
	if len(args) < 3 {
		return e.encode(wrongArguments(name), false)
	}
	z, err := e.parseZRangeArguments(args, false, kind, dir)
	if err != nil {
		return e.encode(err, false)
	}
	zs, ok := e.zsetFor(args[0])
	// A keyspace hit or a miss, counted after the range and options were
	// read, as Redis's zrangeGenericCommand reads them before it looks the
	// key up.
	e.noteLookup(ok)
	if !ok {
		return constant.RespEmptyArray
	}
	walk := zrangeWalk(zs, z)
	if z.withScores && e.replyRESP3 {
		return e.scoredReply3(walk, true)
	}
	return e.scoredReply(walk, z.withScores)
}

func (e *Engine) cmdZRANGE(args []string) []byte {
	return e.zrangeCommand("ZRANGE", args, zrangeAuto, zrangeDirectionAuto)
}
func (e *Engine) cmdZRANGEBYSCORE(args []string) []byte {
	return e.zrangeCommand("ZRANGEBYSCORE", args, zrangeScore, zrangeForward)
}
func (e *Engine) cmdZREVRANGEBYSCORE(args []string) []byte {
	return e.zrangeCommand("ZREVRANGEBYSCORE", args, zrangeScore, zrangeReverse)
}
func (e *Engine) cmdZREVRANGE(args []string) []byte {
	return e.zrangeCommand("ZREVRANGE", args, zrangeRank, zrangeReverse)
}
func (e *Engine) cmdZRANGEBYLEX(args []string) []byte {
	return e.zrangeCommand("ZRANGEBYLEX", args, zrangeLex, zrangeForward)
}
func (e *Engine) cmdZREVRANGEBYLEX(args []string) []byte {
	return e.zrangeCommand("ZREVRANGEBYLEX", args, zrangeLex, zrangeReverse)
}

// zrangeArgumentChecks are the range commands' refusals of their arguments,
// which Redis reads before the key, for argumentsBeforeType.
func zrangeArgumentCheck(kind zrangeKind, dir zrangeDirection, store bool) func(*Engine, []string) error {
	return func(e *Engine, args []string) error {
		if store {
			args = args[1:]
		}
		_, err := e.parseZRangeArguments(args, store, kind, dir)
		return err
	}
}

func (e *Engine) cmdZINCRBY(args []string) []byte {
	e.recordsItself()
	if len(args) != 3 {
		return e.encode(wrongArguments("ZINCRBY"), false)
	}
	increment, err := parseZScore(args[1])
	if err != nil {
		return e.encode(err, false)
	}
	score, err := e.opZIncrBy(args[0], increment, args[2])
	if err != nil {
		return e.encode(err, false)
	}
	return e.encode(ReplyDouble(formatZScore(score)), false)
}

func (e *Engine) cmdZPOPMIN(args []string) []byte { return e.zpop("ZPOPMIN", args, false) }

// zpopCount reads ZPOPMIN's and ZPOPMAX's optional count, refused as Redis
// refuses it before the key is looked at.
func (e *Engine) zpopCount(name string, args []string) (int, error) {
	if len(args) < 1 {
		return 0, wrongArguments(name)
	}
	if len(args) > 2 {
		return 0, errSyntax
	}
	if len(args) == 1 {
		return 1, nil
	}
	n, err := e.positiveCount(args[1])
	return int(n), err
}
func (e *Engine) cmdZPOPMAX(args []string) []byte { return e.zpop("ZPOPMAX", args, true) }

// zpop pops the count lowest or highest members, recording the ZREM of what
// it popped and nothing when it popped nothing.
func (e *Engine) zpop(name string, args []string, reverse bool) []byte {
	e.recordsItself()
	count, err := e.zpopCount(name, args)
	if err != nil {
		return e.encode(err, false)
	}
	z, ok := e.zsetFor(args[0])
	if !ok || count == 0 {
		return constant.RespEmptyArray
	}
	count = min(count, z.Len())
	walk := func(yield func(string, float64) bool) { z.VisitRangeByRank(0, count-1, reverse, yield) }
	names := replyWalk(func(yield func(string) bool) { walk(func(member string, _ float64) bool { return yield(member) }) })
	if refusal := e.reserveRemoval("ZREM", args[0], count, names); refusal != nil {
		return refusal
	}
	// Given a count, RESP3 nests each member with its score; without one the
	// single pair stays flat, as it does in Redis.
	var out []byte
	if e.replyRESP3 {
		out = e.scoredReply3(walk, len(args) == 2)
	} else {
		out = e.scoredReply(walk, true)
	}
	if len(out) > 0 && out[0] == '-' {
		return out
	}
	record := make([]string, 2, count+2)
	record[0], record[1] = "ZREM", args[0]
	names(func(member string) bool { record = append(record, member); return true })
	for _, member := range record[2:] {
		z.Remove(member)
	}
	e.zsetSettle(args[0], z)
	e.aofRecord(record...)
	return out
}
