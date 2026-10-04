package core

import (
	"errors"
	"math"
	"strings"

	"github.com/brandopakel/keel/internal/constant"
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
		return Encode(wrongArguments("ZCOUNT"), false)
	}
	r, err := parseScoreInterval(args[1], args[2])
	if err != nil {
		return Encode(err, false)
	}
	z, ok := e.zsetFor(args[0])
	if !ok {
		return constant.RespZero
	}
	return Encode(z.CountByScore(r.min, r.max, r.minEx, r.maxEx), false)
}

func (e *Engine) cmdZRANGEBYSCORE(args []string) []byte {
	return e.scoreRange("ZRANGEBYSCORE", args, false)
}
func (e *Engine) cmdZREVRANGEBYSCORE(args []string) []byte {
	return e.scoreRange("ZREVRANGEBYSCORE", args, true)
}

// parseScoreRange reads ZRANGEBYSCORE's and ZREVRANGEBYSCORE's arguments in
// Redis's order, the options before the range.
func parseScoreRange(args []string, reverse bool) (r scoreInterval, withScores bool, offset, count int, err error) {
	count = -1
	for i := 3; i < len(args); i++ {
		switch opt := strings.ToUpper(args[i]); {
		case opt == "WITHSCORES":
			withScores = true
		case opt == "LIMIT" && i+2 < len(args):
			if offset, count, err = integerRange(args[i+1], args[i+2]); err != nil {
				return r, false, 0, 0, err
			}
			i += 2
		default:
			return r, false, 0, 0, errSyntax
		}
	}
	lower, upper := args[1], args[2]
	if reverse {
		lower, upper = upper, lower
	}
	r, err = parseScoreInterval(lower, upper)
	return r, withScores, offset, count, err
}

func (e *Engine) scoreRange(name string, args []string, reverse bool) []byte {
	if len(args) < 3 {
		return Encode(wrongArguments(name), false)
	}
	r, withScores, offset, count, err := parseScoreRange(args, reverse)
	if err != nil {
		return Encode(err, false)
	}
	return e.scoreRangeReply(args[0], r, offset, count, reverse, withScores)
}

// scoreRangeReply answers the members of key whose scores are within r.
func (e *Engine) scoreRangeReply(key string, r scoreInterval, offset, count int, reverse, withScores bool) []byte {
	z, ok := e.zsetFor(key)
	if !ok {
		return constant.RespEmptyArray
	}
	walk := func(yield func(string, float64) bool) {
		z.VisitRangeByScore(r.min, r.max, r.minEx, r.maxEx, offset, count, reverse, yield)
	}
	if withScores && replyRESP3 {
		return e.scoredReply3(walk, true)
	}
	return e.scoredReply(walk, withScores)
}

func (e *Engine) cmdZINCRBY(args []string) []byte {
	if len(args) != 3 {
		return Encode(wrongArguments("ZINCRBY"), false)
	}
	increment, err := parseZScore(args[1])
	if err != nil {
		return Encode(err, false)
	}
	z, ok := e.zsetFor(args[0])
	score := increment
	if ok {
		if old, present := z.Score(args[2]); present {
			score += old
		}
	}
	if math.IsNaN(score) {
		return Encode(errors.New("ERR resulting score is not a number (NaN)"), false)
	}
	e.zaddApply(args[0], []float64{score}, []string{args[2]}, 0)
	// Canonical commands keep the log readable by older Keel versions.
	aofRecord("ZADD", args[0], formatZScore(score), args[2])
	return Encode(ReplyDouble(formatZScore(score)), false)
}

func (e *Engine) cmdZPOPMIN(args []string) []byte { return e.zpop("ZPOPMIN", args, false) }

// zpopCount reads ZPOPMIN's and ZPOPMAX's optional count, refused as Redis
// refuses it before the key is looked at.
func zpopCount(name string, args []string) (int, error) {
	if len(args) < 1 {
		return 0, wrongArguments(name)
	}
	if len(args) > 2 {
		return 0, errSyntax
	}
	if len(args) == 1 {
		return 1, nil
	}
	n, err := positiveCount(args[1])
	return int(n), err
}
func (e *Engine) cmdZPOPMAX(args []string) []byte { return e.zpop("ZPOPMAX", args, true) }
func (e *Engine) zpop(name string, args []string, reverse bool) []byte {
	count, err := zpopCount(name, args)
	if err != nil {
		return Encode(err, false)
	}
	z, ok := e.zsetFor(args[0])
	if !ok || count == 0 {
		aof.skip = true
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
	if replyRESP3 {
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
	aofRecord(record...)
	return out
}
