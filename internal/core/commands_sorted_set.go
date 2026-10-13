package core

import (
	"errors"
	"math"
	"strconv"
	"strings"

	"github.com/brandopakel/keel/internal/data_structure"
)

// Sorted set commands: the command language over the typed operations of
// zsets.go. The range reads keep their walks and scored replies.

var (
	errNotAFloat = errors.New("ERR value is not a valid float")
	errNXWithXX  = errors.New("ERR XX and NX options at the same time are not compatible")
	errSyntax    = errors.New("ERR syntax error")
	errGTLTNX    = errors.New("ERR GT, LT, and/or NX options at the same time are not compatible")
	errIncrPairs = errors.New("ERR INCR option supports a single increment-element pair")
)

// zaddOptions reads the flags that may precede ZADD's score/member pairs -
// NX, XX, CH, INCR, GT and LT, in any order and case - and returns where the
// pairs begin.
func zaddOptions(args []string) (flags int, ch bool, next int) {
	for next < len(args) {
		switch strings.ToUpper(args[next]) {
		case "NX":
			flags |= data_structure.ZAddNX
		case "XX":
			flags |= data_structure.ZAddXX
		case "CH":
			ch = true
		case "INCR":
			flags |= data_structure.ZAddIncr
		case "GT":
			flags |= data_structure.ZAddGT
		case "LT":
			flags |= data_structure.ZAddLT
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

// zaddShape reads ZADD's options in Redis's order, then whether what follows
// them comes in score/member pairs, then whether the options agree, and
// returns the pairs (t_zset.c, zaddGenericCommand). The scores are read by
// the caller, after this.
func zaddShape(args []string) (flags int, ch bool, pairs []string, err error) {
	flags, ch, next := zaddOptions(args[1:])
	pairs = args[1+next:]
	if len(pairs) == 0 || len(pairs)%2 != 0 {
		return 0, false, nil, errSyntax
	}
	nx, xx := flags&data_structure.ZAddNX != 0, flags&data_structure.ZAddXX != 0
	gt, lt := flags&data_structure.ZAddGT != 0, flags&data_structure.ZAddLT != 0
	if nx && xx {
		return 0, false, nil, errNXWithXX
	}
	if (gt && nx) || (lt && nx) || (gt && lt) {
		return 0, false, nil, errGTLTNX
	}
	if flags&data_structure.ZAddIncr != 0 && len(pairs) > 2 {
		return 0, false, nil, errIncrPairs
	}
	return flags, ch, pairs, nil
}

// zaddArguments is ZADD's refusal of its arguments, or nil: its shape, and
// then every score, as Redis reads them before it looks at the key.
func zaddArguments(args []string) error {
	_, _, pairs, err := zaddShape(args)
	for i := 0; err == nil && i < len(pairs); i += 2 {
		_, err = parseZScore(pairs[i])
	}
	return err
}

// cmdZADD implements ZADD key [NX|XX] [GT|LT] [CH] [INCR] score member
// [score member ...].
//
// The reply is the number of members added, or with CH the number added or
// rescored; with INCR, the member's new score, or nil when an option ruled it
// out. Every score is checked before any is applied, so a bad one leaves the
// set exactly as it was.
func (e *Engine) cmdZADD(args []string) []byte {
	e.recordsItself()
	if len(args) < 3 {
		return e.encode(wrongArguments("ZADD"), false)
	}
	flags, ch, pairs, err := zaddShape(args)
	if err != nil {
		return e.encode(err, false)
	}
	req := zaddRequest{flags: flags, scores: make([]float64, 0, len(pairs)/2),
		members: make([]string, 0, len(pairs)/2), text: args}
	for i := 0; i < len(pairs); i += 2 {
		score, err := parseZScore(pairs[i])
		if err != nil {
			return e.encode(err, false)
		}
		req.scores = append(req.scores, score)
		req.members = append(req.members, pairs[i+1])
	}
	res, err := e.opZAdd(args[0], req)
	if err != nil {
		return e.encode(err, false)
	}
	if flags&data_structure.ZAddIncr != 0 {
		if !res.processed {
			return e.nullReply()
		}
		return e.encode(ReplyDouble(formatZScore(res.score)), false)
	}
	r := e.reply()
	if ch {
		return r.Int(res.changed).Bytes()
	}
	return r.Int(res.added).Bytes()
}

// zrankArguments reads ZRANK's and ZREVRANK's optional WITHSCORE as Redis
// does: more arguments than that are the wrong number, and any other word
// there a syntax error, both before the key is looked at.
func zrankArguments(name string, args []string) (withScore bool, err error) {
	if len(args) > 3 {
		return false, wrongArguments(name)
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
// score, or a null array. ZREVRANK counts from the highest.
func (e *Engine) cmdZRANK(args []string) []byte    { return e.zrank("ZRANK", args, false) }
func (e *Engine) cmdZREVRANK(args []string) []byte { return e.zrank("ZREVRANK", args, true) }

func (e *Engine) zrank(name string, args []string, reverse bool) []byte {
	if len(args) < 2 {
		return e.encode(wrongArguments(name), false)
	}
	withScore, err := zrankArguments(name, args)
	if err != nil {
		return e.encode(err, false)
	}
	missing := e.nullReply()
	if withScore {
		missing = e.nullArrayReply()
	}
	zs, ok := e.zsetFor(args[0])
	// A keyspace hit or a miss, counted after WITHSCORE was read, as Redis's
	// zrankGenericCommand reads it before it looks the key up.
	e.noteLookup(ok)
	if !ok {
		return missing
	}
	rank, ok := zs.Rank(args[1], reverse)
	if !ok {
		return missing
	}
	if withScore {
		score, _ := zs.Score(args[1])
		return e.encode([]interface{}{int64(rank), ReplyDouble(formatZScore(score))}, false)
	}
	return e.encode(rank, false)
}

func (e *Engine) cmdZREM(args []string) []byte {
	e.recordsItself()
	if len(args) < 2 {
		return e.encode(wrongArguments("ZREM"), false)
	}
	r := e.reply()
	return r.Int(e.opZRem(args[0], args[1:])).Bytes()
}

// cmdZSCORE answers a member's score - a double in RESP3, a bulk string in
// RESP2 - or nil when the member or the key is absent.
func (e *Engine) cmdZSCORE(args []string) []byte {
	if len(args) != 2 {
		return e.encode(wrongArguments("ZSCORE"), false)
	}
	score, ok := e.opZScore(args[0], args[1])
	if !ok {
		return e.nullReply()
	}
	return e.encode(ReplyDouble(formatZScore(score)), false)
}

func (e *Engine) cmdZCARD(args []string) []byte {
	if len(args) != 1 {
		return e.encode(wrongArguments("ZCARD"), false)
	}
	r := e.reply()
	return r.Int(e.opZCard(args[0])).Bytes()
}
