package core

import (
	"math"
	"math/big"
	"strings"

	"github.com/brandopakel/keel/internal/resp"
)

// The command language of the string commands Redis 8 added: DIGEST,
// DELEX, MSETEX and INCREX (SET's conditions are in cmdSET). Each reads its
// arguments in the order Redis 8.10.1 reads them, calls its typed operation
// (strings_redis8.go), and answers through the reply sink.

// cmdDIGEST implements DIGEST key: the XXH3 digest of the string key holds,
// in 16 hexadecimal digits, or null for none. A key of another type is
// refused before the handler runs.
func (e *Engine) cmdDIGEST(args []string) []byte {
	if len(args) != 1 {
		return e.encode(wrongArguments("DIGEST"), false)
	}
	r := e.reply()
	if digest, ok := e.opDigest(args[0]); ok {
		return r.Bulk(digest).Bytes()
	}
	return r.Null().Bytes()
}

// cmdDELEX implements DELEX key [IFEQ value | IFNE value | IFDEQ digest |
// IFDNE digest]: DEL of one key, or of a string key that meets the
// condition. Any count but one key or a key and a condition with its value
// is refused as the wrong number of arguments.
func (e *Engine) cmdDELEX(args []string) []byte {
	e.recordsItself()
	if len(args) != 1 && len(args) != 3 {
		return e.encode(wrongArguments("DELEX"), false)
	}
	r := e.reply()
	var deleted bool
	var err error
	if len(args) == 1 {
		deleted, err = e.opDelex(args[0], "", "", false)
	} else {
		deleted, err = e.opDelex(args[0], args[1], args[2], true)
	}
	if err != nil {
		return r.Error(err).Bytes()
	}
	if deleted {
		return r.Int(1).Bytes()
	}
	return r.Int(0).Bytes()
}

// cmdMSETEX implements MSETEX numkeys key value [key value ...] [NX | XX]
// [EX seconds | PX milliseconds | EXAT unix-time-seconds | PXAT
// unix-time-milliseconds | KEEPTTL]: 1 when it stored every pair, 0 when NX
// or XX declined and it stored none.
func (e *Engine) cmdMSETEX(args []string) []byte {
	e.recordsItself()
	if len(args) < 3 {
		return e.encode(wrongArguments("MSETEX"), false)
	}
	pairs, req, err := e.msetexArguments(args)
	if err != nil {
		return e.encode(err, false)
	}
	r := e.reply()
	if e.opMSetEx(pairs, req) {
		return r.Int(1).Bytes()
	}
	return r.Int(0).Bytes()
}

// msetexArguments reads MSETEX's arguments in Redis's order: the count of
// pairs, from 1 to INT_MAX, then whether that many pairs follow, then the
// options after them, then the expiry's value.
func (e *Engine) msetexArguments(args []string) ([]string, msetexRequest, error) {
	n, valid := e.counterInteger(args[0])
	if !valid || n < 1 || n > math.MaxInt32 {
		return nil, msetexRequest{}, errMsetexNumkeys
	}
	if n*2+1 > int64(len(args)) {
		return nil, msetexRequest{}, errKeyValuePairs
	}
	end := 1 + 2*int(n)
	set, _, _, err := e.setArguments("MSETEX", args[end:], false)
	if err != nil {
		return nil, msetexRequest{}, err
	}
	req := msetexRequest{nx: set.nx, xx: set.xx, set: set}
	req.set.nx, req.set.xx = false, false
	// MSETEX stores each key with its expiry whatever the instant, as
	// Redis's msetexCommand calls setKey and setExpire with no
	// checkAlreadyExpired (t_string.c), unlike SET (lines 161-174): a key
	// whose EXAT or PXAT has passed is stored, and removed, and counted as
	// expired, when it is next found.
	req.set.absolute = false
	return args[1:end], req, nil
}

// msetexKeys are the keys an MSETEX names, for the rewrite's and the
// replicas' tracking of what a command wrote: none when its count is not
// one the command accepts. It runs on dispatch, before MSETEX reads its
// arguments, so the count is bounded by dividing the arguments rather than
// by doubling the count, which overflows for a count near the int64 limit.
func msetexKeys(args []string) []string {
	n, valid := canonicalInteger(args[0])
	if !valid || n < 1 || n > int64((len(args)-1)/2) {
		return nil
	}
	keys := make([]string, 0, n)
	for i := 1; i < 1+2*int(n); i += 2 {
		keys = append(keys, args[i])
	}
	return keys
}

// cmdINCREX implements INCREX key [BYFLOAT increment | BYINT increment]
// [LBOUND lowerbound] [UBOUND upperbound] [SATURATE] [EX seconds | PX
// milliseconds | EXAT unix-time-seconds | PXAT unix-time-milliseconds |
// PERSIST] [ENX]. It answers the value after the increment and the
// increment applied: integers, or doubles with BYFLOAT. The reply is
// admitted before anything is written.
func (e *Engine) cmdINCREX(args []string) []byte {
	e.recordsItself()
	if len(args) < 1 {
		return e.encode(wrongArguments("INCREX"), false)
	}
	req, err := e.increxArguments(args[1:])
	if err != nil {
		return e.encode(err, false)
	}
	result, err := e.increxSum(args[0], req)
	r := e.reply()
	if err != nil {
		return r.Error(err).Bytes()
	}
	if !e.increxReply(&r, req, result) {
		return r.Bytes()
	}
	e.storeIncrex(args[0], req, result)
	return r.Bytes()
}

// increxReply writes INCREX's two values into r, sized and admitted first,
// and reports whether they were admitted.
func (e *Engine) increxReply(r *resp.Reply, req increxRequest, result increxResult) bool {
	size := 4 // *2\r\n
	if req.byFloat {
		size, _ = r.Framing.AddDoubleSize(size, len(result.valueF))
		size, _ = r.Framing.AddDoubleSize(size, len(result.appliedF))
		if !r.Reserve(size) {
			return false
		}
		r.Array(2).Double(result.valueF).Double(result.appliedF)
		return true
	}
	size += integerReplySize(result.value) + integerReplySize(result.applied)
	if !r.Reserve(size) {
		return false
	}
	r.Array(2).Int(result.value).Int(result.applied)
	return true
}

// integerReplySize is the encoded size of :n\r\n.
func integerReplySize(n int64) int {
	if n < 0 {
		if n == math.MinInt64 {
			return 23
		}
		return 4 + decimalDigits(int(-n))
	}
	return 3 + decimalDigits(int(n))
}

// increxArguments reads INCREX's options as Redis's
// parseIncrExArgumentsOrReply reads them: in order, the increment's value as
// soon as it is met, then the bounds, read as the increment's type once
// every option is known, then ENX's need for an expiry, then the expiry's
// value. Each option may be given once, BYINT and BYFLOAT exclude each
// other, and so do the expiries and PERSIST, and ENX and PERSIST.
func (e *Engine) increxArguments(opts []string) (increxRequest, error) {
	req := increxRequest{incr: 1, lower: math.MinInt64, upper: math.MaxInt64}
	var lower, upper, unit, expire string
	var hasIncrement, hasLower, hasUpper bool
	for i := 0; i < len(opts); i++ {
		opt := strings.ToUpper(opts[i])
		next := i+1 < len(opts)
		switch {
		case opt == "BYINT" && next && !hasIncrement:
			n, valid := e.counterInteger(opts[i+1])
			if !valid {
				return req, errIncrexIncrInteger
			}
			req.incr, hasIncrement = n, true
			i++
		case opt == "BYFLOAT" && next && !hasIncrement:
			f, valid := parseLongDouble(opts[i+1])
			if !valid {
				return req, errIncrexIncrFloat
			}
			if f.IsInf() {
				return req, errIncrexIncrInfinite
			}
			req.byFloat, req.incrF, hasIncrement = true, f, true
			i++
		case opt == "LBOUND" && next && !hasLower:
			lower, hasLower = opts[i+1], true
			i++
		case opt == "UBOUND" && next && !hasUpper:
			upper, hasUpper = opts[i+1], true
			i++
		case opt == "SATURATE" && !req.saturate:
			req.saturate = true
		case opt == "ENX" && !req.enx && !req.persist:
			req.enx = true
		case opt == "PERSIST" && unit == "" && !req.persist && !req.enx:
			req.persist = true
		case (opt == "EX" || opt == "PX" || opt == "EXAT" || opt == "PXAT") && unit == "" && !req.persist && next:
			unit, expire = opt, opts[i+1]
			i++
		default:
			return req, errSyntax
		}
	}
	if err := e.increxBounds(&req, lower, upper, hasLower, hasUpper); err != nil {
		return req, err
	}
	if req.enx && unit == "" {
		return req, errIncrexENX
	}
	if unit != "" {
		at, err := e.expiryArgument("INCREX", unit, expire)
		if err != nil {
			return req, err
		}
		req.expires, req.at = true, at
	}
	return req, nil
}

// increxBounds reads LBOUND and UBOUND as the increment's type: long
// doubles with BYFLOAT, from -LDBL_MAX to LDBL_MAX when not given, and
// integers otherwise.
func (e *Engine) increxBounds(req *increxRequest, lower, upper string, hasLower, hasUpper bool) error {
	if req.byFloat {
		req.lowerF, req.upperF = new(big.Float).Neg(ldblMax), ldblMax
		if hasLower {
			f, valid := parseLongDouble(lower)
			if !valid {
				return errIncrexLowerFloat
			}
			req.lowerF = f
		}
		if hasUpper {
			f, valid := parseLongDouble(upper)
			if !valid {
				return errIncrexUpperFloat
			}
			req.upperF = f
		}
		if req.lowerF.Cmp(req.upperF) > 0 {
			return errIncrexBounds
		}
		return nil
	}
	if hasLower {
		n, valid := e.counterInteger(lower)
		if !valid {
			return errIncrexLowerInteger
		}
		req.lower = n
	}
	if hasUpper {
		n, valid := e.counterInteger(upper)
		if !valid {
			return errIncrexUpperInteger
		}
		req.upper = n
	}
	if req.lower > req.upper {
		return errIncrexBounds
	}
	return nil
}
