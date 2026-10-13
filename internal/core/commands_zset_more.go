package core

import (
	"errors"
	"math/rand/v2"
	"strings"

	"github.com/brandopakel/keel/internal/constant"
	"github.com/brandopakel/keel/internal/data_structure"
)

// The sorted-set commands step 4.7 adds: ZMSCORE, ZLEXCOUNT, the
// ZREMRANGEBY* family, ZRANDMEMBER, ZRANGESTORE and ZMPOP. The range
// commands (ZREVRANGE, ZRANGEBYLEX, ZREVRANGEBYLEX, ZRANGE's BYLEX) are in
// commands_zrange_score.go, ZREVRANK beside ZRANK.

var (
	errMpopNumkeys   = errors.New("ERR numkeys should be greater than 0")
	errCountPositive = errors.New("ERR count should be greater than 0")
	errOutOfRange    = errors.New("ERR value is out of range")
)

// cmdZMSCORE answers each member's score, or nil for one that is not there,
// as Redis's zmscoreCommand does: a missing key answers nil for every member.
func (e *Engine) cmdZMSCORE(args []string) []byte {
	if len(args) < 2 {
		return e.encode(wrongArguments("ZMSCORE"), false)
	}
	zs, ok := e.zsetFor(args[0])
	reply := make([]interface{}, len(args)-1)
	for i, member := range args[1:] {
		if !ok {
			continue
		}
		if score, present := zs.Score(member); present {
			reply[i] = ReplyDouble(formatZScore(score))
		}
	}
	return e.encode(reply, false)
}

// cmdZLEXCOUNT counts the members inside a lexicographic range.
func (e *Engine) cmdZLEXCOUNT(args []string) []byte {
	if len(args) != 3 {
		return e.encode(wrongArguments("ZLEXCOUNT"), false)
	}
	r, err := parseLexInterval(args[1], args[2])
	if err != nil {
		return e.encode(err, false)
	}
	zs, ok := e.zsetFor(args[0])
	// Counted after the range was read, as Redis's zlexcountCommand reads it
	// before it looks the key up.
	e.noteLookup(ok)
	reply := e.reply()
	if !ok {
		return reply.Int(0).Bytes()
	}
	return reply.Int(zs.CountByLex(r)).Bytes()
}

// zremrangeArguments reads ZREMRANGEBYRANK's, ZREMRANGEBYSCORE's and
// ZREMRANGEBYLEX's range, refused in Redis's words before the key is looked
// at (t_zset.c, zremrangeGenericCommand).
func (e *Engine) zremrangeArguments(args []string, kind zrangeKind) (z zrangeArguments, err error) {
	z.kind, z.count = kind, -1
	switch kind {
	case zrangeScore:
		z.scores, err = parseScoreInterval(args[1], args[2])
	case zrangeLex:
		z.lex, err = parseLexInterval(args[1], args[2])
	default:
		z.start, z.stop, err = e.integerRange(args[1], args[2])
	}
	return z, err
}

func (e *Engine) cmdZREMRANGEBYRANK(args []string) []byte {
	return e.zremrange("ZREMRANGEBYRANK", args, zrangeRank)
}
func (e *Engine) cmdZREMRANGEBYSCORE(args []string) []byte {
	return e.zremrange("ZREMRANGEBYSCORE", args, zrangeScore)
}
func (e *Engine) cmdZREMRANGEBYLEX(args []string) []byte {
	return e.zremrange("ZREMRANGEBYLEX", args, zrangeLex)
}

// zremrange removes a range of ranks, scores or members and answers how many
// it removed.
func (e *Engine) zremrange(name string, args []string, kind zrangeKind) []byte {
	e.recordsItself()
	if len(args) != 3 {
		return e.encode(wrongArguments(name), false)
	}
	z, err := e.zremrangeArguments(args, kind)
	if err != nil {
		return e.encode(err, false)
	}
	reply := e.reply()
	return reply.Int(e.opZRemRange(args[0], z, name, args)).Bytes()
}

// cmdZRANDMEMBER implements ZRANDMEMBER key [count [WITHSCORES]], as Redis's
// zrandmemberCommand does: without a count, one member or nil; with a
// positive count, up to that many distinct members; with a negative one,
// exactly that many, drawn independently.
func (e *Engine) cmdZRANDMEMBER(args []string) []byte {
	if len(args) < 1 {
		return e.encode(wrongArguments("ZRANDMEMBER"), false)
	}
	count, given, withScores, err := e.zrandmemberArguments(args)
	if err != nil {
		return e.encode(err, false)
	}
	zs, ok := e.zsetFor(args[0])
	// Counted after the count was read, as Redis's zrandmemberCommand reads
	// it before it looks the key up.
	e.noteLookup(ok)
	if !given {
		if !ok {
			return e.nullReply()
		}
		member, _, _ := zs.MemberAtRank(rand.IntN(zs.Len()))
		return e.encode(member, false)
	}
	if !ok || count == 0 {
		return constant.RespEmptyArray
	}
	var ranks []int
	if count < 0 {
		if -count > maxRandomMemberCount {
			return e.encode(errIntegerOutOfRange, false)
		}
		ranks = make([]int, -count)
		for i := range ranks {
			ranks[i] = rand.IntN(zs.Len())
		}
	} else {
		ranks = rand.Perm(zs.Len())[:min(int(min(count, maxRandomMemberCount)), zs.Len())]
	}
	walk := func(yield func(string, float64) bool) {
		for _, rank := range ranks {
			member, score, _ := zs.MemberAtRank(rank)
			if !yield(member, score) {
				return
			}
		}
	}
	if withScores && e.replyRESP3 {
		return e.scoredReply3(walk, true)
	}
	return e.scoredReply(walk, withScores)
}

// zrandmemberArguments reads ZRANDMEMBER's count and WITHSCORES in Redis's
// order: the count as SRANDMEMBER reads one, then more arguments or another
// word than WITHSCORES a syntax error, then a count WITHSCORES would make too
// large to answer.
func (e *Engine) zrandmemberArguments(args []string) (count int64, given, withScores bool, err error) {
	count, given, err = e.randomCount(args)
	if err != nil || !given {
		return count, given, false, err
	}
	if len(args) > 3 || (len(args) == 3 && !strings.EqualFold(args[2], "WITHSCORES")) {
		return 0, true, false, errSyntax
	}
	if len(args) == 3 {
		withScores = true
		const half = int64(^uint64(0)>>1) / 2
		if count < -half || count > half {
			return 0, true, false, errOutOfRange
		}
	}
	return count, given, withScores, nil
}

// cmdZRANGESTORE implements ZRANGESTORE dst src min max [BYSCORE|BYLEX] [REV]
// [LIMIT offset count]: src's range, as ZRANGE reads it, stored at dst in
// place of whatever dst held, and the number stored answered. An empty range
// deletes dst. It is recorded as sent, when dst changed, as Redis propagates
// it.
func (e *Engine) cmdZRANGESTORE(args []string) []byte {
	e.recordsItself()
	if len(args) < 4 {
		return e.encode(wrongArguments("ZRANGESTORE"), false)
	}
	z, err := e.parseZRangeArguments(args[1:], true, zrangeAuto, zrangeDirectionAuto)
	if err != nil {
		return e.encode(err, false)
	}
	zs, err := e.zsetSource(args[1])
	if err != nil {
		return e.encode(err, false)
	}
	var members []string
	var scores []float64
	if zs != nil {
		zrangeWalk(zs, z)(func(member string, score float64) bool {
			members, scores = append(members, member), append(scores, score)
			return true
		})
	}
	if e.opZStore(args[0], members, scores) {
		e.aofRecordArgs("ZRANGESTORE", args)
	}
	reply := e.reply()
	return reply.Int(int64(len(members))).Bytes()
}

// zsetSource looks key up as a sorted set some command reads, counting the
// lookup as a keyspace hit or a miss, as Redis's lookupKeyRead does: nil for
// a key that is not there, and Redis's refusal of a key of another type.
// The commands that read one this way check its type themselves, rather than
// through the type check, because the other keys they name are not held to a
// type: ZRANGESTORE's destination is replaced whatever it holds.
func (e *Engine) zsetSource(key string) (*data_structure.ZSet, error) {
	owner, held := e.space.OwnerOf(key)
	e.noteLookup(held)
	if !held {
		return nil, nil
	}
	if owner.KeyspaceName() != e.zsetStore.KeyspaceName() {
		return nil, errWrongType
	}
	zs, _ := e.zsetFor(key)
	return zs, nil
}

// zmpopArguments reads ZMPOP numkeys key [key ...] MIN|MAX [COUNT count] in
// Redis's order (t_zset.c, zmpopGenericCommand): numkeys, then MIN or MAX
// after the keys, then COUNT once.
func (e *Engine) zmpopArguments(args []string) (keys []string, max bool, count int, err error) {
	// getRangeLongFromObjectOrReply answers a value that is not an integer
	// in the words it was given for one out of range.
	n, valid := e.counterInteger(args[0])
	if !valid || n < 1 {
		return nil, false, 0, errMpopNumkeys
	}
	if n >= int64(len(args)-1) {
		return nil, false, 0, errSyntax
	}
	keys = args[1 : 1+n]
	switch strings.ToUpper(args[1+n]) {
	case "MIN":
	case "MAX":
		max = true
	default:
		return nil, false, 0, errSyntax
	}
	count = -1
	for i := int(2 + n); i < len(args); i++ {
		if count == -1 && strings.EqualFold(args[i], "COUNT") && i+1 < len(args) {
			i++
			c, valid := e.counterInteger(args[i])
			if !valid || c < 1 {
				return nil, false, 0, errCountPositive
			}
			count = int(min(c, int64(maxRandomMemberCount)))
			continue
		}
		return nil, false, 0, errSyntax
	}
	if count == -1 {
		count = 1
	}
	return keys, max, count, nil
}

// cmdZMPOP pops up to count of the lowest (MIN) or highest (MAX) members of
// the first of keys that holds a sorted set, and answers that key and the
// members with their scores; a null array when none does. A key of another
// type before it is refused, as Redis's genericZpopCommand looks the keys up
// in order and stops at the first that exists. It records the ZREM of what
// it popped, as ZPOPMIN does, and nothing when it popped nothing.
func (e *Engine) cmdZMPOP(args []string) []byte {
	e.recordsItself()
	if len(args) < 3 {
		return e.encode(wrongArguments("ZMPOP"), false)
	}
	keys, max, count, err := e.zmpopArguments(args)
	if err != nil {
		return e.encode(err, false)
	}
	for _, key := range keys {
		owner, held := e.space.OwnerOf(key)
		if !held {
			continue
		}
		if owner.KeyspaceName() != e.zsetStore.KeyspaceName() {
			return e.encode(errWrongType, false)
		}
		zs, _ := e.zsetFor(key)
		return e.zmpopFrom(key, zs, count, max)
	}
	return e.nullArrayReply()
}

// zmpopFrom pops from zs and answers ZMPOP's reply: the key, then each
// member and its score as a pair, in both protocols.
func (e *Engine) zmpopFrom(key string, zs *data_structure.ZSet, count int, max bool) []byte {
	count = min(count, zs.Len())
	walk := func(yield func(string, float64) bool) { zs.VisitRangeByRank(0, count-1, max, yield) }
	names := replyWalk(func(yield func(string) bool) { walk(func(member string, _ float64) bool { return yield(member) }) })
	if refusal := e.reserveRemoval("ZREM", key, count, names); refusal != nil {
		return refusal
	}
	f := e.framing
	size, fits := addBulkSize(4, len(key))
	header := decimalDigits(count) + 3
	walk(func(member string, score float64) bool {
		size += 4 // each pair is an array of two, in both protocols
		if size, fits = addBulkSize(size, len(member)); fits {
			size, fits = f.addDoubleSize(size, len(formatZScore(score)))
		}
		return fits
	})
	if !fits || size > MaxReplyBytes-header {
		return replyTooLarge
	}
	size += header
	if refusal := e.admitReply(size); refusal != nil {
		return refusal
	}
	out := appendArrayHeader(make([]byte, 0, size), 2)
	out = appendBulkString(out, key)
	out = appendArrayHeader(out, count)
	popped := make([]string, 0, count)
	walk(func(member string, score float64) bool {
		out = appendArrayHeader(out, 2)
		out = appendBulkString(out, member)
		out = appendDouble(f, out, formatZScore(score))
		popped = append(popped, member)
		return true
	})
	for _, member := range popped {
		zs.Remove(member)
	}
	e.zsetSettle(key, zs)
	e.aofRecordKeyArgs("ZREM", key, popped)
	return out
}
