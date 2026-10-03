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

// zaddArguments reads ZADD's arguments in Redis's order: the options, then
// whether what follows them comes in score/member pairs, then whether the
// options agree, then every score, so a malformed command is refused before
// the key is looked at and with the error Redis would give it.
func zaddArguments(args []string) (flags int, ch bool, scores []float64, members []string, err error) {
	flags, ch, next := zaddOptions(args[1:])
	pairs := args[1+next:]
	if len(pairs) == 0 || len(pairs)%2 != 0 {
		return 0, false, nil, nil, errSyntax
	}
	if flags&data_structure.ZAddNX != 0 && flags&data_structure.ZAddXX != 0 {
		return 0, false, nil, nil, errNXWithXX
	}
	scores = make([]float64, 0, len(pairs)/2)
	members = make([]string, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		score, err := parseZScore(pairs[i])
		if err != nil {
			return 0, false, nil, nil, err
		}
		scores = append(scores, score)
		members = append(members, pairs[i+1])
	}
	return flags, ch, scores, members, nil
}

// cmdZADD implements ZADD key [NX|XX] [CH] score member [score member ...].
//
// The reply is the number of members added, or with CH the number added or
// rescored. Every score is checked before any is applied, so a bad one leaves
// the set exactly as it was.
func cmdZADD(args []string) []byte {
	if len(args) < 3 {
		return Encode(wrongArguments("ZADD"), false)
	}
	flags, ch, scores, members, err := zaddArguments(args)
	if err != nil {
		return Encode(err, false)
	}
	added, changed := zaddApply(args[0], scores, members, flags)
	if ch {
		return Encode(changed, false)
	}
	return Encode(added, false)
}

// zrankArguments reads ZRANK's optional WITHSCORE as Redis does: more
// arguments than that are the wrong number, and any other word there a syntax
// error, both before the key is looked at.
func zrankArguments(args []string) (withScore bool, err error) {
	if len(args) > 3 {
		return false, wrongArguments("ZRANK")
	}
	if len(args) == 3 {
		if !strings.EqualFold(args[2], "WITHSCORE") {
			return false, errSyntax
		}
		withScore = true
	}
	return withScore, nil
}

// cmdZRANK answers a member's 0-based position from the lowest score, or nil
// for a member or key that is not there; with WITHSCORE, the position and the
// score, or a null array.
func cmdZRANK(args []string) []byte {
	if len(args) < 2 {
		return Encode(wrongArguments("ZRANK"), false)
	}
	withScore, err := zrankArguments(args)
	if err != nil {
		return Encode(err, false)
	}
	missing := nullReply
	if withScore {
		missing = nullArrayReply
	}
	zs, ok := zsetFor(args[0])
	if !ok {
		return missing()
	}
	rank, ok := zs.Rank(args[1], false)
	if !ok {
		return missing()
	}
	if withScore {
		score, _ := zs.Score(args[1])
		return Encode([]interface{}{int64(rank), ReplyDouble(formatZScore(score))}, false)
	}
	return Encode(rank, false)
}

func cmdZREM(args []string) []byte {
	if len(args) < 2 {
		return Encode(wrongArguments("ZREM"), false)
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

// cmdZSCORE answers a member's score - a double in RESP3, a bulk string in
// RESP2 - or nil when the member or the key is absent.
func cmdZSCORE(args []string) []byte {
	if len(args) != 2 {
		return Encode(wrongArguments("ZSCORE"), false)
	}
	zs, ok := zsetFor(args[0])
	if !ok {
		return nullReply()
	}
	score, ok := zs.Score(args[1])
	if !ok {
		return nullReply()
	}
	return Encode(ReplyDouble(formatZScore(score)), false)
}

func cmdZCARD(args []string) []byte {
	if len(args) != 1 {
		return Encode(wrongArguments("ZCARD"), false)
	}
	zs, ok := zsetFor(args[0])
	if !ok {
		return constant.RespZero
	}
	return Encode(zs.Len(), false)
}

// zrangeArguments is how ZRANGE was asked for its range.
type zrangeArguments struct {
	byScore, reverse, withScores bool
	// offset and count are LIMIT's, count -1 when it was not given.
	offset, count int
	start, stop   int
	scores        scoreInterval
}

// parseZRange reads ZRANGE key start stop [BYSCORE] [REV] [LIMIT offset count]
// [WITHSCORES] in Redis's order: every option first, then whether they
// agree, then the range itself, as ranks or, with BYSCORE, as scores given
// from the first end walked. BYLEX is not offered, and is refused with the
// syntax error of any other word this server does not know there.
func parseZRange(args []string) (z zrangeArguments, err error) {
	z.count = -1
	for i := 3; i < len(args); i++ {
		switch opt := strings.ToUpper(args[i]); {
		case opt == "WITHSCORES":
			z.withScores = true
		case opt == "LIMIT" && i+2 < len(args):
			if z.offset, z.count, err = integerRange(args[i+1], args[i+2]); err != nil {
				return z, err
			}
			i += 2
		case opt == "REV" && !z.reverse:
			z.reverse = true
		case opt == "BYSCORE" && !z.byScore:
			z.byScore = true
		default:
			return z, errSyntax
		}
	}
	if z.count != -1 && !z.byScore {
		return z, errors.New("ERR syntax error, LIMIT is only supported in combination with either BYSCORE or BYLEX")
	}
	if z.byScore {
		lower, upper := args[1], args[2]
		if z.reverse {
			lower, upper = upper, lower
		}
		z.scores, err = parseScoreInterval(lower, upper)
		return z, err
	}
	z.start, z.stop, err = integerRange(args[1], args[2])
	return z, err
}

// cmdZRANGE answers a range of ranks, or of scores with BYSCORE, in either
// direction, with or without scores.
func cmdZRANGE(args []string) []byte {
	if len(args) < 3 {
		return Encode(wrongArguments("ZRANGE"), false)
	}
	z, err := parseZRange(args)
	if err != nil {
		return Encode(err, false)
	}
	if z.byScore {
		return scoreRangeReply(args[0], z.scores, z.offset, z.count, z.reverse, z.withScores)
	}
	zs, ok := zsetFor(args[0])
	if !ok {
		return constant.RespEmptyArray
	}
	walk := func(yield func(string, float64) bool) { zs.VisitRangeByRank(z.start, z.stop, z.reverse, yield) }
	if z.withScores && replyRESP3 {
		return scoredReply3(walk, true)
	}
	return scoredReply(walk, z.withScores)
}
