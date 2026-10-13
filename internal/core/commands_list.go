package core

import (
	"errors"
	"math"
	"strings"

	"github.com/brandopakel/keel/internal/constant"
	"github.com/brandopakel/keel/internal/resp"
)

// The list commands' command language: each reads its arguments as Redis
// reads them, calls its typed operation (lists.go), and answers through the
// engine's reply sink. A write records itself; it is never logged as it
// arrived.

// push is LPUSH and RPUSH, which differ only in the end they add to.
func (e *Engine) push(args []string, front bool, name string) []byte {
	e.recordsItself()
	if len(args) < 2 {
		return e.encode(wrongArguments(name), false)
	}
	r := e.reply()
	return r.Int(e.opPush(args[0], args[1:], front)).Bytes()
}

func (e *Engine) cmdLPUSH(args []string) []byte { return e.push(args, true, "LPUSH") }
func (e *Engine) cmdRPUSH(args []string) []byte { return e.push(args, false, "RPUSH") }

// pop is LPOP and RPOP. Without a count it answers one element; with one it
// answers an array, which is Redis 6.2's behaviour and the one clients test
// for - a count of zero is an empty array rather than a nil.
//
// The count is read before the key is looked at, as Redis reads it. The
// elements are answered, and their reply admitted, before they are removed:
// a reply that cannot be sent leaves the list as it was.
func (e *Engine) pop(args []string, front bool, name string) []byte {
	e.recordsItself()
	if len(args) < 1 || len(args) > 2 {
		return e.encode(wrongArguments(name), false)
	}
	req := listPop{front: front, counted: len(args) == 2}
	if req.counted {
		n, err := e.positiveCount(args[1])
		if err != nil {
			return e.encode(err, false)
		}
		req.count = n
	}
	key := args[0]

	l, ok := e.listFor(key)
	if !ok {
		// A null array and a null bulk string are different replies, and which
		// one belongs here depends on what the command was going to answer.
		// Checked against Redis 8.10.1, which sends *-1 for the counted form
		// and $-1 for the bare one; sending $-1 for both is a type error in any
		// client that decodes the counted reply into a list. RESP3 has one
		// null for both.
		if req.counted {
			return e.wire().NullArray()
		}
		return e.wire().Null()
	}

	if !req.counted {
		// One element is a reply of one bulk string, answered as GET's is
		// (resp.SingleBulk).
		end := 0
		if !front {
			end = -1
		}
		value, _ := l.Index(end)
		out := resp.SingleBulk(MaxReplyBytes-e.replyCeiling, e.commandAllocations, value)
		if out[0] == '-' {
			return out
		}
		e.opPop(key, l, req)
		return out
	}

	count := req.popped(l.Len())
	out := e.encodeWalkReply(func(yield func(string) bool) {
		for i := 0; i < count; i++ {
			index := i
			if !front {
				index = l.Len() - 1 - i
			}
			value, _ := l.Index(index)
			if !yield(value) {
				return
			}
		}
	}, shapeArray)
	if len(out) > 0 && out[0] == '-' {
		return out
	}
	e.opPop(key, l, req)
	return out
}

func (e *Engine) cmdLPOP(args []string) []byte { return e.pop(args, true, "LPOP") }
func (e *Engine) cmdRPOP(args []string) []byte { return e.pop(args, false, "RPOP") }

func (e *Engine) cmdLLEN(args []string) []byte {
	if len(args) != 1 {
		return e.encode(wrongArguments("LLEN"), false)
	}
	r := e.reply()
	return r.Int(e.opLLen(args[0])).Bytes()
}

// cmdLINDEX answers a key that is not there with nil whatever the index says,
// because Redis looks the key up before it reads the index.
func (e *Engine) cmdLINDEX(args []string) []byte {
	if len(args) != 2 {
		return e.encode(wrongArguments("LINDEX"), false)
	}
	index, valid := e.counterInteger(args[1])
	if !valid {
		if _, ok := e.listFor(args[0]); !ok {
			return e.wire().Null()
		}
		return e.encode(errNotAnInteger, false)
	}
	// One element, answered as GET's is (resp.SingleBulk).
	if value, ok := e.opLIndex(args[0], index); ok {
		return resp.SingleBulk(MaxReplyBytes-e.replyCeiling, e.commandAllocations, value)
	}
	return e.wire().Null()
}

// cmdLSET, like LINDEX, refuses a key that is not there before it reads the
// index.
func (e *Engine) cmdLSET(args []string) []byte {
	e.recordsItself()
	if len(args) != 3 {
		return e.encode(wrongArguments("LSET"), false)
	}
	r := e.reply()
	index, valid := e.counterInteger(args[1])
	if !valid {
		if _, ok := e.listFor(args[0]); !ok {
			return r.Error(errNoSuchKey).Bytes()
		}
		return r.Error(errNotAnInteger).Bytes()
	}
	if err := e.opLSet(args[0], index, args[2]); err != nil {
		return r.Error(err).Bytes()
	}
	return r.Status("OK").Bytes()
}

// cmdLRANGE answers the elements between two positions, both inclusive and both
// allowed to be negative or out of range - a range outside the list is empty
// rather than an error, which is what makes LRANGE key 0 -1 the idiom for
// "everything" whatever the length. The positions are read before the key is
// looked at, as Redis reads them.
func (e *Engine) cmdLRANGE(args []string) []byte {
	if len(args) != 3 {
		return e.encode(wrongArguments("LRANGE"), false)
	}
	start, stop, err := e.integerRange(args[1], args[2])
	if err != nil {
		return e.encode(err, false)
	}
	rng, ok := e.opLRange(args[0], start, stop)
	if !ok {
		return constant.RespEmptyArray
	}
	return e.encodeWalkReply(rng.visit, shapeArray)
}

// cmdLTRIM reads its positions before the key, as LRANGE does; see opLTrim.
func (e *Engine) cmdLTRIM(args []string) []byte {
	e.recordsItself()
	if len(args) != 3 {
		return e.encode(wrongArguments("LTRIM"), false)
	}
	start, stop, err := e.integerRange(args[1], args[2])
	if err != nil {
		return e.encode(err, false)
	}
	e.opLTrim(args[0], start, stop)
	r := e.reply()
	return r.Status("OK").Bytes()
}

// The list commands Keel lacked, as Redis 8.10.1's t_list.c defines them
// (phase 4, the lists' missing commands).

var (
	// errRankZero refuses LPOS's RANK 0, in Redis's words.
	errRankZero = errors.New("ERR RANK can't be zero: use 1 to start from the first match, 2 from the second ... or use negative to start from the end of the list")
	// errLPosCount and errLPosMaxLen refuse a COUNT or MAXLEN that is not a
	// whole number of zero or more, in the words LPOS gives
	// getPositiveLongFromObjectOrReply.
	errLPosCount  = errors.New("ERR COUNT can't be negative")
	errLPosMaxLen = errors.New("ERR MAXLEN can't be negative")
	// errNumkeys refuses LMPOP's numkeys below one or not a number.
	errNumkeys = errors.New("ERR numkeys should be greater than 0")
	// errCountPositive refuses LMPOP's and LMOVEM's count below one or not a
	// number.
	errCountPositive = errors.New("ERR count should be greater than 0")
)

func (e *Engine) cmdLPUSHX(args []string) []byte { return e.pushX(args, true, "LPUSHX") }
func (e *Engine) cmdRPUSHX(args []string) []byte { return e.pushX(args, false, "RPUSHX") }

// pushX is LPUSHX and RPUSHX: LPUSH and RPUSH onto a list that exists.
func (e *Engine) pushX(args []string, front bool, name string) []byte {
	e.recordsItself()
	if len(args) < 2 {
		return e.encode(wrongArguments(name), false)
	}
	r := e.reply()
	return r.Int(e.opPushX(args[0], args[1:], front)).Bytes()
}

// linsertWhere reads LINSERT's BEFORE or AFTER, in any case.
func linsertWhere(text string) (after bool, err error) {
	switch {
	case strings.EqualFold(text, "AFTER"):
		return true, nil
	case strings.EqualFold(text, "BEFORE"):
		return false, nil
	}
	return false, errSyntax
}

// cmdLINSERT reads BEFORE or AFTER before it looks at the key, as Redis
// does; see opLInsert.
func (e *Engine) cmdLINSERT(args []string) []byte {
	e.recordsItself()
	if len(args) != 4 {
		return e.encode(wrongArguments("LINSERT"), false)
	}
	after, err := linsertWhere(args[1])
	if err != nil {
		return e.encode(err, false)
	}
	r := e.reply()
	return r.Int(e.opLInsert(args[0], after, args[2], args[3])).Bytes()
}

// signedLong reads an integer as Redis's getRangeLongFromObjectOrReply reads
// one between -LONG_MAX and LONG_MAX with no message of its own: LREM's count
// and LPOS's RANK.
func (e *Engine) signedLong(text string) (int64, error) {
	n, valid := e.counterInteger(text)
	switch {
	case !valid:
		return 0, errNotAnInteger
	case n == math.MinInt64:
		return 0, errRandomCountRange
	}
	return n, nil
}

// atLeast reads an integer of at least least, refusing anything else with
// refusal, as getRangeLongFromObjectOrReply does when given a message.
func (e *Engine) atLeast(text string, least int64, refusal error) (int64, error) {
	n, valid := e.counterInteger(text)
	if !valid || n < least {
		return 0, refusal
	}
	return n, nil
}

// cmdLREM reads its count before it looks at the key, as Redis does; see
// opLRem.
func (e *Engine) cmdLREM(args []string) []byte {
	e.recordsItself()
	if len(args) != 3 {
		return e.encode(wrongArguments("LREM"), false)
	}
	count, err := e.signedLong(args[1])
	if err != nil {
		return e.encode(err, false)
	}
	r := e.reply()
	return r.Int(e.opLRem(args[0], count, args[2])).Bytes()
}

// lposArguments reads LPOS's options as Redis reads them: each may be given
// again, and its last value counts; one without a value, or a word that is
// none of them, is a syntax error.
func (e *Engine) lposArguments(opts []string) (lposRequest, error) {
	req := lposRequest{rank: 1, count: -1}
	for j := 0; j < len(opts); j++ {
		more := j+1 < len(opts)
		var err error
		switch opt := opts[j]; {
		case strings.EqualFold(opt, "RANK") && more:
			j++
			if req.rank, err = e.signedLong(opts[j]); err == nil && req.rank == 0 {
				err = errRankZero
			}
		case strings.EqualFold(opt, "COUNT") && more:
			j++
			req.count, err = e.atLeast(opts[j], 0, errLPosCount)
		case strings.EqualFold(opt, "MAXLEN") && more:
			j++
			req.maxLen, err = e.atLeast(opts[j], 0, errLPosMaxLen)
		default:
			err = errSyntax
		}
		if err != nil {
			return req, err
		}
	}
	return req, nil
}

// cmdLPOS reads its options before it looks at the key, as Redis does, and
// answers a position or nil, or with COUNT an array of them, which a key
// holding no list answers empty; see opLPos.
func (e *Engine) cmdLPOS(args []string) []byte {
	if len(args) < 2 {
		return e.encode(wrongArguments("LPOS"), false)
	}
	req, err := e.lposArguments(args[2:])
	if err != nil {
		return e.encode(err, false)
	}
	matches, ok := e.opLPos(args[0], args[1], req)
	r := e.reply()
	if req.count == -1 {
		at := int64(-1)
		if ok {
			matches.visit(func(i int64) bool { at = i; return false })
		}
		if at < 0 {
			return r.Null().Bytes()
		}
		return r.Int(at).Bytes()
	}
	if !ok {
		return constant.RespEmptyArray
	}
	// An array of positions, sized by a first pass and admitted once.
	n, size := 0, 0
	matches.visit(func(i int64) bool {
		n++
		size += resp.DecimalDigits(int(i)) + 3
		return true
	})
	if !r.Reserve(size + resp.DecimalDigits(n) + 3) {
		return r.Bytes()
	}
	r.Array(n)
	matches.visit(func(i int64) bool { r.Int(i); return true })
	return r.Bytes()
}

// lmoveEnds reads LMOVE's and LMOVEM's two ends, the source's and then the
// destination's.
func lmoveEnds(args []string) (listMove, error) {
	from, err := listEnd(args[2])
	if err != nil {
		return listMove{}, err
	}
	to, err := listEnd(args[3])
	return listMove{fromFront: from, toFront: to}, err
}

// cmdLMOVE reads its ends before it looks at a key, as Redis does; see
// moveSource and opLMove.
func (e *Engine) cmdLMOVE(args []string) []byte {
	e.recordsItself()
	if len(args) != 4 {
		return e.encode(wrongArguments("LMOVE"), false)
	}
	req, err := lmoveEnds(args)
	if err != nil {
		return e.encode(err, false)
	}
	return e.move(args[0], args[1], req, false)
}

// cmdRPOPLPUSH is LMOVE source destination RIGHT LEFT, Redis's older name
// for it.
func (e *Engine) cmdRPOPLPUSH(args []string) []byte {
	e.recordsItself()
	if len(args) != 2 {
		return e.encode(wrongArguments("RPOPLPUSH"), false)
	}
	return e.move(args[0], args[1], listMove{toFront: true}, true)
}

// move answers LMOVE and RPOPLPUSH: the element moved, admitted before it
// moves, or nil when the source holds no list.
func (e *Engine) move(src, dst string, req listMove, rpoplpush bool) []byte {
	l, _, err := e.moveSource(src, dst, func(n int) int { return min(1, n) })
	switch {
	case err != nil:
		return e.encode(err, false)
	case l == nil:
		return e.wire().Null()
	}
	end := 0
	if !req.fromFront {
		end = -1
	}
	value, _ := l.Index(end)
	out := resp.SingleBulk(MaxReplyBytes-e.replyCeiling, e.commandAllocations, value)
	if out[0] == '-' {
		return out
	}
	if rpoplpush {
		e.opRPopLPush(src, l, dst)
	} else {
		e.opLMove(src, l, dst, req)
	}
	return out
}

// lmovemArguments reads LMOVEM's ends and its optional trailer, which is
// exactly three words when given: COUNT or EXACTLY, a count of at least one,
// and OBO or BULK (Redis's lmovemParseOptions).
func (e *Engine) lmovemArguments(args []string) (lmovemRequest, error) {
	ends, err := lmoveEnds(args)
	if err != nil {
		return lmovemRequest{}, err
	}
	req := lmovemRequest{listMove: ends, count: 1}
	opts := args[4:]
	if len(opts) == 0 {
		return req, nil
	}
	if len(opts) != 3 {
		return req, errSyntax
	}
	switch {
	case strings.EqualFold(opts[0], "COUNT"):
		req.mode = lmovemUpTo
	case strings.EqualFold(opts[0], "EXACTLY"):
		req.mode = lmovemExactly
	default:
		return req, errSyntax
	}
	if req.count, err = e.atLeast(opts[1], 1, errCountPositive); err != nil {
		return req, err
	}
	switch {
	case strings.EqualFold(opts[2], "BULK"):
		req.bulk = true
	case !strings.EqualFold(opts[2], "OBO"):
		return req, errSyntax
	}
	return req, nil
}

// cmdLMOVEM moves one element, up to count (COUNT) or exactly count
// (EXACTLY), and answers them as an array in the order they read in the
// destination, admitted before they move; nil when it moves none. See
// moveSource and opLMoveM.
func (e *Engine) cmdLMOVEM(args []string) []byte {
	e.recordsItself()
	if len(args) < 4 {
		return e.encode(wrongArguments("LMOVEM"), false)
	}
	req, err := e.lmovemArguments(args)
	if err != nil {
		return e.encode(err, false)
	}
	src, dst := args[0], args[1]
	l, n, err := e.moveSource(src, dst, req.moves)
	switch {
	case err != nil:
		return e.encode(err, false)
	case n == 0:
		return e.wire().NullArray()
	}
	out := e.encodeWalkReply(func(yield func(string) bool) {
		for i := 0; i < n; i++ {
			if !yield(req.movedElement(l, n, i)) {
				return
			}
		}
	}, shapeArray)
	if out[0] == '-' {
		return out
	}
	e.opLMoveM(src, l, dst, req, n)
	return out
}

// lmpopArguments reads LMPOP's arguments in Redis's order: numkeys, the end
// after the keys it counts, and an optional COUNT of at least one, given at
// most once (lmpopGenericCommand).
func (e *Engine) lmpopArguments(args []string) (lmpopRequest, error) {
	var req lmpopRequest
	numkeys, err := e.atLeast(args[0], 1, errNumkeys)
	if err != nil {
		return req, err
	}
	if numkeys >= int64(len(args)-1) {
		return req, errSyntax
	}
	where := 1 + int(numkeys)
	if req.front, err = listEnd(args[where]); err != nil {
		return req, err
	}
	req.keys, req.count = args[1:where], -1
	for j := where + 1; j < len(args); j++ {
		if req.count != -1 || !strings.EqualFold(args[j], "COUNT") || j+1 == len(args) {
			return req, errSyntax
		}
		j++
		if req.count, err = e.atLeast(args[j], 1, errCountPositive); err != nil {
			return req, err
		}
	}
	if req.count == -1 {
		req.count = 1
	}
	return req, nil
}

// cmdLMPOP pops from the first of its keys that holds a list, and answers
// that key and the elements it popped, admitted before they are removed; a
// null array when no key holds one. A key of another type is refused when
// it is reached, as Redis reaches it. See opLMPop.
func (e *Engine) cmdLMPOP(args []string) []byte {
	e.recordsItself()
	if len(args) < 3 {
		return e.encode(wrongArguments("LMPOP"), false)
	}
	req, err := e.lmpopArguments(args)
	if err != nil {
		return e.encode(err, false)
	}
	key, l, ok, err := e.lmpopSource(req.keys)
	switch {
	case err != nil:
		return e.encode(err, false)
	case !ok:
		return e.wire().NullArray()
	}
	n := int(min(req.count, int64(l.Len())))
	element := func(i int) string {
		if !req.front {
			i = l.Len() - 1 - i
		}
		v, _ := l.Index(i)
		return v
	}
	// [key, [elements]], sized and admitted before anything is popped.
	size, fits := resp.AddBulkSize(4, len(key))
	size += resp.DecimalDigits(n) + 3
	for i := 0; i < n && fits; i++ {
		size, fits = resp.AddBulkSize(size, len(element(i)))
	}
	if !fits {
		size = MaxReplyBytes + 1
	}
	r := e.reply()
	if !r.Reserve(size) {
		return r.Bytes()
	}
	r.Array(2).Bulk(key).Array(n)
	for i := 0; i < n; i++ {
		r.Bulk(element(i))
	}
	e.opLMPop(key, l, req.front, req.count)
	return r.Bytes()
}
