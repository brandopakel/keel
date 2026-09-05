package core

import (
	"errors"
	"math"
	"strconv"
	"strings"

	"github.com/brandopakel/keel/internal/constant"
	"github.com/brandopakel/keel/internal/data_structure"
)

// Sorted set commands.
//
// Redis has no empty sorted set: removing the last member removes the key, and
// a ZADD that adds nothing to a key that did not exist leaves no key behind.
// Every command that changes a set in place goes through zsetSettle, which
// applies that rule and re-measures the set for the memory budget.

func zsetFor(key string) (*data_structure.ZSet, bool) {
	return zsetStore.Get(key)
}

// zsetSettle records that a sorted set was changed in place: it is dropped when
// empty and re-measured otherwise.
func zsetSettle(key string, zs *data_structure.ZSet) {
	if zs.Len() == 0 {
		zsetStore.Delete(key)
		return
	}
	zsetStore.Resize(key)
}

var (
	errNotAFloat = errors.New("ERR value is not a valid float")
	errNXWithXX  = errors.New("ERR XX and NX options at the same time are not compatible")
	errSyntax    = errors.New("ERR syntax error")
)

// zaddOptions reads the flags that may precede the score/member pairs of ZADD
// and GEOADD, and returns where the pairs begin.
func zaddOptions(args []string) (flags int, ch bool, next int) {
	for next < len(args) {
		switch strings.ToUpper(args[next]) {
		case "NX":
			flags |= data_structure.ZAddNX
		case "XX":
			flags |= data_structure.ZAddXX
		case "CH":
			ch = true
		default:
			return flags, ch, next
		}
		next++
	}
	return flags, ch, next
}

// parseZScore reads a score the way ZADD accepts one: any float, infinities
// included, but never NaN, which has no place in an ordering.
func parseZScore(s string) (float64, error) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) {
		return 0, errNotAFloat
	}
	return f, nil
}

// formatZScore writes a score the way Redis 7.2 replies with one, which is
// its d2string: the infinities by name, zero with its sign, an integral value
// within 2^62 as an integer, and anything else with the fewest digits that
// read back to the same float, laid out by the rules of its fpconv_dtoa - a
// point for numbers near one, an exponent for numbers far from it. A fixed
// number of decimals would print 0.0000001 as 0.000000 and give a geohash
// score six zeros it never had.
func formatZScore(score float64) string {
	switch {
	case math.IsInf(score, 1):
		return "inf"
	case math.IsInf(score, -1):
		return "-inf"
	case score == 0:
		if math.Signbit(score) {
			return "-0"
		}
		return "0"
	}
	// Redis prints an integral value as an integer when it is safely one:
	// within half the range of a 64-bit integer, and unchanged by the round
	// trip through one.
	const safeInteger = float64(math.MaxInt64 / 2)
	if score >= -safeInteger && score <= safeInteger {
		if n := int64(score); float64(n) == score {
			return strconv.FormatInt(n, 10)
		}
	}
	return shortestDouble(score)
}

// shortestDouble lays out the shortest round-tripping digits of a float the
// way fpconv_dtoa does. With the digits D (n of them) and the value D x 10^K:
//
//	K >= 0 and the exponent below n+7      the digits, then K zeros
//	K < 0, and K > -7 or the exponent < 4  a decimal point, with leading
//	                                       zeros for a value below one
//	otherwise                              d.ddde<sign><exponent>, the
//	                                       exponent without padding
//
// where the exponent is that of scientific notation, taken absolute.
func shortestDouble(v float64) string {
	var b strings.Builder
	if v < 0 {
		b.WriteByte('-')
		v = -v
	}
	// Go's 'e' form with -1 precision is the shortest digit string that reads
	// back to the same float, which is what Grisu2 finds in all but rare cases.
	mantissa, expText, _ := strings.Cut(strconv.FormatFloat(v, 'e', -1, 64), "e")
	digits := strings.Replace(mantissa, ".", "", 1)
	sciExp, _ := strconv.Atoi(expText)
	n := len(digits)
	k := sciExp - (n - 1)
	exp := sciExp
	if exp < 0 {
		exp = -exp
	}

	switch {
	case k >= 0 && exp < n+7:
		b.WriteString(digits)
		b.WriteString(strings.Repeat("0", k))
	case k < 0 && (k > -7 || exp < 4):
		if offset := n + k; offset <= 0 {
			b.WriteString("0.")
			b.WriteString(strings.Repeat("0", -offset))
			b.WriteString(digits)
		} else {
			b.WriteString(digits[:offset])
			b.WriteByte('.')
			b.WriteString(digits[offset:])
		}
	default:
		b.WriteByte(digits[0])
		if n > 1 {
			b.WriteByte('.')
			b.WriteString(digits[1:])
		}
		b.WriteByte('e')
		if sciExp < 0 {
			b.WriteByte('-')
		} else {
			b.WriteByte('+')
		}
		b.WriteString(strconv.Itoa(exp))
	}
	return b.String()
}

// zaddApply adds every score/member pair to the set at key under flags, and
// reports how many members were new and how many were new or rescored.
//
// The set is created on demand, except under XX, where nothing new may be added
// and so a key that does not exist stays that way.
func zaddApply(key string, scores []float64, members []string, flags int) (added, changed int) {
	zs, ok := zsetFor(key)
	if !ok {
		if flags&data_structure.ZAddXX != 0 {
			return 0, 0
		}
		zs = data_structure.CreateZSet()
		zsetStore.Put(key, zs)
	}
	for i, score := range scores {
		switch zs.Add(score, members[i], flags) {
		case data_structure.ZAddAdded:
			added++
			changed++
		case data_structure.ZAddUpdated:
			changed++
		}
	}
	zsetSettle(key, zs)
	return added, changed
}

// cmdZADD implements ZADD key [NX|XX] [CH] score member [score member ...].
//
// The reply is the number of members added, or with CH the number added or
// rescored. Every score is checked before any is applied, so a bad one leaves
// the set exactly as it was.
func cmdZADD(args []string) []byte {
	if len(args) < 3 {
		return Encode(errors.New("ERR wrong number of arguments for 'ZADD' command"), false)
	}
	key := args[0]
	flags, ch, next := zaddOptions(args[1:])
	if flags&data_structure.ZAddNX != 0 && flags&data_structure.ZAddXX != 0 {
		return Encode(errNXWithXX, false)
	}
	pairs := args[1+next:]
	if len(pairs) == 0 || len(pairs)%2 != 0 {
		return Encode(errSyntax, false)
	}

	scores := make([]float64, 0, len(pairs)/2)
	members := make([]string, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		score, err := parseZScore(pairs[i])
		if err != nil {
			return Encode(err, false)
		}
		scores = append(scores, score)
		members = append(members, pairs[i+1])
	}

	added, changed := zaddApply(key, scores, members, flags)
	if ch {
		return Encode(changed, false)
	}
	return Encode(added, false)
}

// cmdZRANK answers a member's 0-based position from the lowest score, or nil
// for a member or key that is not there.
func cmdZRANK(args []string) []byte {
	if len(args) != 2 {
		return Encode(errors.New("ERR wrong number of arguments for 'ZRANK' command"), false)
	}
	zs, ok := zsetFor(args[0])
	if !ok {
		return constant.RespNil
	}
	rank, ok := zs.Rank(args[1], false)
	if !ok {
		return constant.RespNil
	}
	return Encode(rank, false)
}

func cmdZREM(args []string) []byte {
	if len(args) < 2 {
		return Encode(errors.New("ERR wrong number of arguments for 'ZREM' command"), false)
	}
	key := args[0]
	zs, ok := zsetFor(key)
	if !ok {
		return constant.RespZero
	}
	removed := 0
	for _, member := range args[1:] {
		if zs.Remove(member) {
			removed++
		}
	}
	zsetSettle(key, zs)
	return Encode(removed, false)
}

// cmdZSCORE answers a member's score as a bulk string, or nil when the member
// or the key is absent.
func cmdZSCORE(args []string) []byte {
	if len(args) != 2 {
		return Encode(errors.New("ERR wrong number of arguments for 'ZSCORE' command"), false)
	}
	zs, ok := zsetFor(args[0])
	if !ok {
		return constant.RespNil
	}
	score, ok := zs.Score(args[1])
	if !ok {
		return constant.RespNil
	}
	return Encode(formatZScore(score), false)
}

func cmdZCARD(args []string) []byte {
	if len(args) != 1 {
		return Encode(errors.New("ERR wrong number of arguments for 'ZCARD' command"), false)
	}
	zs, ok := zsetFor(args[0])
	if !ok {
		return constant.RespZero
	}
	return Encode(zs.Len(), false)
}

// cmdZRANGE supports rank ranges, REV and WITHSCORES; score/lex ranges are rejected.
func cmdZRANGE(args []string) []byte {
	if len(args) < 3 {
		return Encode(errSyntax, false)
	}
	start, e1 := strconv.Atoi(args[1])
	stop, e2 := strconv.Atoi(args[2])
	if e1 != nil || e2 != nil {
		return Encode(errNotAnInteger, false)
	}
	reverse, withScores := false, false
	for _, opt := range args[3:] {
		switch strings.ToUpper(opt) {
		case "REV":
			reverse = true
		case "WITHSCORES":
			withScores = true
		default:
			return Encode(errSyntax, false)
		}
	}
	zs, ok := zsetFor(args[0])
	if !ok {
		return constant.RespEmptyArray
	}
	members, scores := zs.RangeByRank(start, stop, reverse)
	if len(members) == 0 {
		return constant.RespEmptyArray
	}
	if !withScores {
		return Encode(members, false)
	}
	out := make([]string, 0, len(members)*2)
	for i, m := range members {
		out = append(out, m, formatZScore(scores[i]))
	}
	return Encode(out, false)
}
