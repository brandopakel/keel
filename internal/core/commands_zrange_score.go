package core

import (
	"errors"
	"math"
	"strconv"
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

func cmdZCOUNT(args []string) []byte {
	if len(args) != 3 {
		return Encode(errSyntax, false)
	}
	min, minEx, e1 := scoreBound(args[1])
	max, maxEx, e2 := scoreBound(args[2])
	if e1 != nil || e2 != nil {
		return Encode(errNotAFloat, false)
	}
	z, ok := zsetFor(args[0])
	if !ok {
		return constant.RespZero
	}
	return Encode(z.CountByScore(min, max, minEx, maxEx), false)
}

func cmdZRANGEBYSCORE(args []string) []byte    { return scoreRange(args, false) }
func cmdZREVRANGEBYSCORE(args []string) []byte { return scoreRange(args, true) }
func scoreRange(args []string, reverse bool) []byte {
	if len(args) < 3 {
		return Encode(errSyntax, false)
	}
	lower, upper := args[1], args[2]
	if reverse {
		lower, upper = upper, lower
	}
	min, minEx, e1 := scoreBound(lower)
	max, maxEx, e2 := scoreBound(upper)
	if e1 != nil || e2 != nil {
		return Encode(errNotAFloat, false)
	}
	withScores := false
	offset, count := 0, -1
	for i := 3; i < len(args); i++ {
		switch strings.ToUpper(args[i]) {
		case "WITHSCORES":
			withScores = true
		case "LIMIT":
			if i+2 >= len(args) {
				return Encode(errSyntax, false)
			}
			var e1, e2 error
			offset, e1 = strconv.Atoi(args[i+1])
			count, e2 = strconv.Atoi(args[i+2])
			i += 2
			if e1 != nil || e2 != nil {
				return Encode(errNotAnInteger, false)
			}
		default:
			return Encode(errSyntax, false)
		}
	}
	z, ok := zsetFor(args[0])
	if !ok {
		return constant.RespEmptyArray
	}
	members, scores := z.RangeByScore(min, max, minEx, maxEx, offset, count, reverse)
	return encodeScoredMembers(members, scores, withScores)
}

func encodeScoredMembers(members []string, scores []float64, withScores bool) []byte {
	if len(members) == 0 {
		return constant.RespEmptyArray
	}
	if !withScores {
		return Encode(members, false)
	}
	out := make([]string, 0, 2*len(members))
	for i, m := range members {
		out = append(out, m, formatZScore(scores[i]))
	}
	return Encode(out, false)
}

func cmdZINCRBY(args []string) []byte {
	if len(args) != 3 {
		return Encode(errSyntax, false)
	}
	increment, err := parseZScore(args[1])
	if err != nil {
		return Encode(err, false)
	}
	z, ok := zsetFor(args[0])
	score := increment
	if ok {
		if old, present := z.Score(args[2]); present {
			score += old
		}
	}
	if math.IsNaN(score) {
		return Encode(errors.New("ERR resulting score is not a number (NaN)"), false)
	}
	zaddApply(args[0], []float64{score}, []string{args[2]}, 0)
	// Canonical commands keep the log readable by older Keel versions.
	aofRecord("ZADD", args[0], formatZScore(score), args[2])
	return Encode(formatZScore(score), false)
}

func cmdZPOPMIN(args []string) []byte { return zpop(args, false) }
func cmdZPOPMAX(args []string) []byte { return zpop(args, true) }
func zpop(args []string, reverse bool) []byte {
	if len(args) < 1 || len(args) > 2 {
		return Encode(errSyntax, false)
	}
	count := 1
	if len(args) == 2 {
		var err error
		count, err = strconv.Atoi(args[1])
		if err != nil || count < 0 {
			return Encode(errNotAnInteger, false)
		}
	}
	z, ok := zsetFor(args[0])
	if !ok || count == 0 {
		aof.skip = true
		return constant.RespEmptyArray
	}
	count = min(count, z.Len())
	members, scores := z.RangeByRank(0, count-1, reverse)
	for _, m := range members {
		z.Remove(m)
	}
	zsetSettle(args[0], z)
	aofRecord(append([]string{"ZREM", args[0]}, members...)...)
	return encodeScoredMembers(members, scores, true)
}
