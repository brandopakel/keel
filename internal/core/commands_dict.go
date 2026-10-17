package core

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/brandopakel/keel/internal/constant"
	"github.com/brandopakel/keel/internal/data_structure"
	"github.com/brandopakel/keel/internal/resp"
)

// String commands, and the expiry commands, which apply to strings here: a TTL
// lives in the string dictionary, and the other types have none.

var (
	errIncrOverflow = errors.New("ERR increment or decrement would overflow")
	errDecrOverflow = errors.New("ERR decrement would overflow")
)

// invalidExpireTime is Redis's refusal of an expiry, which names the command
// that was given it.
func invalidExpireTime(name string) error {
	return fmt.Errorf("ERR invalid expire time in '%s' command", strings.ToLower(name))
}

// The string commands' command language: each reads its arguments as Redis
// reads them, calls its typed operation (strings.go), and answers through
// the engine's reply sink. The operation records what it changed; the
// command is never logged as it arrived.

// SETEX/PSETEX share SET's validation and canonical SET/PEXPIREAT persistence.
// Like SET, they replace a key whatever type held it.
func (e *Engine) cmdSETEX(args []string) []byte  { return e.setWithTTL("SETEX", args, "EX") }
func (e *Engine) cmdPSETEX(args []string) []byte { return e.setWithTTL("PSETEX", args, "PX") }

// cmdSETNX is SET key value NX answering 1 or 0, the older spelling clients
// still send: cachelib's and Flask-Caching's add() are built on it. It is
// recorded as the SET it performs, so a log written here replays on a build
// that predates the name.
func (e *Engine) cmdSETNX(args []string) []byte {
	e.recordsItself()
	if len(args) != 2 {
		return e.encode(wrongArguments("SETNX"), false)
	}
	r := e.reply()
	if e.opSet(e.setTarget(args[0]), args[0], args[1], setRequest{nx: true}) {
		return r.Int(1).Bytes()
	}
	return r.Int(0).Bytes()
}

func (e *Engine) setWithTTL(name string, args []string, unit string) []byte {
	e.recordsItself()
	if len(args) != 3 {
		return e.encode(wrongArguments(name), false)
	}
	at, err := e.expiryArgument(name, unit, args[1])
	if err != nil {
		return e.encode(err, false)
	}
	e.opSet(e.setTarget(args[0]), args[0], args[2], setRequest{expires: true, at: at})
	r := e.reply()
	return r.Status("OK").Bytes()
}

// cmdSET implements SET key value [NX | XX] [GET] [EX seconds | PX
// milliseconds | EXAT unix-time-seconds | PXAT unix-time-milliseconds |
// KEEPTTL].
//
// The expiry keyword used to be skipped over entirely: whatever sat in args[2]
// was ignored and args[3] was read as a number of seconds. So PX asked for
// milliseconds and got seconds, a thousand times longer than the caller wanted
// and silently - the reply was still OK - and a keyword that meant nothing at
// all was accepted just as readily. Anything past EX and PX is refused rather
// than guessed at, which is the difference between a command this server does
// not implement and a command it appears to implement and does not.
func (e *Engine) cmdSET(args []string) []byte {
	e.recordsItself()
	if len(args) < 2 {
		return e.encode(wrongArguments("SET"), false)
	}
	req, get, err := e.setArguments("SET", args[2:])
	if err != nil {
		return e.encode(err, false)
	}
	key, value := args[0], args[1]
	t := e.setTarget(key)
	r := e.reply()
	if get {
		// GET reads the old value as Redis's lookupKeyRead does, so it is a
		// keyspace hit or a miss, a key of another type a hit.
		e.noteLookup(t.exists())
	}
	if get {
		// GET reads the old value as a string, so a key of another type is
		// refused, and its reply is admitted before anything is written: a
		// reply that cannot be sent leaves the key as it was.
		switch {
		case t.other != nil:
			return r.Error(errWrongType).Bytes()
		case t.obj == nil:
			r.Null()
		case r.Bulk(t.obj.Value).Refused():
			return r.Bytes()
		}
	}
	stored := e.opSet(t, key, value, req)
	switch {
	case get:
	case stored:
		r.Status("OK")
	default:
		r.Null()
	}
	return r.Bytes()
}

// layoutPadSetArgs is layout padding for a throwaway control build: never run.
//
//go:noinline
func layoutPadSetArgs(s *[64]uint64) {
	s[0] += s[1] ^ 0x0
	s[7] += s[14] ^ 0x9e3779b1
	s[14] += s[27] ^ 0x3c6ef362
	s[21] += s[40] ^ 0xdaa66d13
	s[28] += s[53] ^ 0x78dde6c4
	s[35] += s[2] ^ 0x17156075
	s[42] += s[15] ^ 0xb54cda26
	s[49] += s[28] ^ 0x538453d7
	s[56] += s[41] ^ 0xf1bbcd88
	s[63] += s[54] ^ 0x8ff34739
	s[6] += s[3] ^ 0x2e2ac0ea
	s[13] += s[16] ^ 0xcc623a9b
	s[20] += s[29] ^ 0x6a99b44c
	s[27] += s[42] ^ 0x8d12dfd
	s[34] += s[55] ^ 0xa708a7ae
	s[41] += s[4] ^ 0x4540215f
	s[48] += s[17] ^ 0xe3779b10
}

// setArguments reads SET's options as Redis reads them: NX and XX exclude
// each other, KEEPTTL and the four expiry options exclude one another, though
// one expiry option may be given again, and its last value counts. The expiry
// is checked only once every option has been read, so a malformed option is
// refused ahead of a malformed expiry; name is the command running, which an
// invalid expiry names.
func (e *Engine) setArguments(name string, opts []string) (req setRequest, get bool, err error) {
	unit, expire := "", ""
	for i := 0; i < len(opts); i++ {
		switch opt := strings.ToUpper(opts[i]); {
		case opt == "NX" && !req.xx:
			req.nx = true
		case opt == "XX" && !req.nx:
			req.xx = true
		case opt == "GET":
			get = true
		case opt == "KEEPTTL" && unit == "":
			req.keepTTL = true
		case (opt == "EX" || opt == "PX" || opt == "EXAT" || opt == "PXAT") && !req.keepTTL &&
			(unit == "" || unit == opt) && i+1 < len(opts):
			unit, expire = opt, opts[i+1]
			i++
		default:
			return req, false, errSyntax
		}
	}
	if unit != "" {
		if req.at, err = e.expiryArgument(name, unit, expire); err != nil {
			return req, false, err
		}
		req.expires, req.absolute = true, unit == "EXAT" || unit == "PXAT"
	}
	return req, get, nil
}

// layoutPadExpiryArg is layout padding for a throwaway control build: never run.
//
//go:noinline
func layoutPadExpiryArg(s *[64]uint64) {
	s[0] += s[1] ^ 0x0
	s[7] += s[14] ^ 0x9e3779b1
	s[14] += s[27] ^ 0x3c6ef362
	s[21] += s[40] ^ 0xdaa66d13
	s[28] += s[53] ^ 0x78dde6c4
	s[35] += s[2] ^ 0x17156075
	s[42] += s[15] ^ 0xb54cda26
	s[49] += s[28] ^ 0x538453d7
	s[56] += s[41] ^ 0xf1bbcd88
	s[63] += s[54] ^ 0x8ff34739
	s[6] += s[3] ^ 0x2e2ac0ea
	s[13] += s[16] ^ 0xcc623a9b
	s[20] += s[29] ^ 0x6a99b44c
	s[27] += s[42] ^ 0x8d12dfd
	s[34] += s[55] ^ 0xa708a7ae
	s[41] += s[4] ^ 0x4540215f
	s[48] += s[17] ^ 0xe3779b10
	s[55] += s[30] ^ 0x81af14c1
	s[62] += s[43] ^ 0x1fe68e72
	s[5] += s[56] ^ 0xbe1e0823
	s[12] += s[5] ^ 0x5c5581d4
	s[19] += s[18] ^ 0xfa8cfb85
	s[26] += s[31] ^ 0x98c47536
	s[33] += s[44] ^ 0x36fbeee7
	s[40] += s[57] ^ 0xd5336898
	s[47] += s[6] ^ 0x736ae249
	s[54] += s[19] ^ 0x11a25bfa
	s[61] += s[32] ^ 0xafd9d5ab
	s[4] += s[45] ^ 0x4e114f5c
	s[11] += s[58] ^ 0xec48c90d
	s[18] += s[7] ^ 0x8a8042be
	s[25] += s[20] ^ 0x28b7bc6f
	s[32] += s[33] ^ 0xc6ef3620
	s[39] += s[46] ^ 0x6526afd1
	s[46] += s[59] ^ 0x35e2982
	s[53] += s[8] ^ 0xa195a333
	s[60] += s[21] ^ 0x3fcd1ce4
	s[3] += s[34] ^ 0xde049695
	s[10] += s[47] ^ 0x7c3c1046
	s[17] += s[60] ^ 0x1a7389f7
}

// expiryArgument reads an expiry given in unit - EX or PX relative to now,
// EXAT or PXAT absolute - as the instant it falls due, in Unix milliseconds.
// It must be a positive integer, and its instant must fit in the signed 64
// bits it is kept in; name is the command running, which Redis's refusal
// names.
func (e *Engine) expiryArgument(name, unit, text string) (int64, error) {
	n, valid := e.counterInteger(text)
	if !valid {
		return 0, errNotAnInteger
	}
	if n <= 0 {
		return 0, invalidExpireTime(name)
	}
	if unit == "EX" || unit == "EXAT" {
		if n > math.MaxInt64/1000 {
			return 0, invalidExpireTime(name)
		}
		n *= 1000
	}
	if unit == "EX" || unit == "PX" {
		at, ok := expiryInstant(n)
		if !ok {
			return 0, invalidExpireTime(name)
		}
		return at, nil
	}
	return n, nil
}

// dropOtherType deletes a key another type holds, for a write that replaces
// it, and stages the DEL that amounts to ahead of the write's own record.
//
// Replay would reach the same state without the DEL, because it runs the same
// SET. It is logged anyway for the build before this one: there SET over a
// hash answers WRONGTYPE, a replay command that fails stops startup, and so a
// rollback could not read a log that relied on SET replacing it.
func (e *Engine) dropOtherType(owner data_structure.Keyspace, key string) {
	owner.Delete(key)
	e.aofRecord("DEL", key)
}

// expiryInstant turns a positive duration in milliseconds into the instant it
// ends, reading the clock once, and reports false when that instant does not
// fit in the signed 64 bits it is kept and compared in.
func expiryInstant(ttlMs int64) (int64, bool) {
	return expiryInstantFrom(time.Now().UnixMilli(), ttlMs)
}

// unixMillis is the wall clock in Unix milliseconds, the engine's
// commandClock.
func unixMillis() int64 { return time.Now().UnixMilli() }

// expiryInstantFrom is expiryInstant from a clock reading the caller already
// took, so that a command can compute an expiry and judge whether it has
// passed against one instant, as Redis's commandTimeSnapshot() does.
func expiryInstantFrom(now, ttlMs int64) (int64, bool) {
	if ttlMs > math.MaxInt64-now {
		return 0, false
	}
	return now + ttlMs, true
}

func (e *Engine) cmdGET(args []string) []byte {
	if len(args) != 1 {
		return e.encode(wrongArguments("GET"), false)
	}
	// GET's reply is its one value, so it is answered without a Reply to
	// build (resp.SingleBulk): the same sizing and admission, measurably
	// cheaper on the hottest read.
	if value, ok := e.opGet(args[0]); ok {
		return resp.SingleBulk(MaxReplyBytes-e.replyCeiling, e.commandAllocations, value)
	}
	return e.wire().Null()
}

// remainingTTL is how long a key has left, in milliseconds. The two negative
// answers are Redis's: -2 for a key that is not there, -1 for one with no
// expiry.
func (e *Engine) remainingTTL(key string) int64 {
	owner, ok := e.space.OwnerOf(key)
	if !ok {
		return -2
	}
	at, has := owner.GetExpiry(key)
	if !has {
		return -1
	}
	return max(0, int64(at)-time.Now().UnixMilli())
}

// cmdTTL answers a key's time to live in whole seconds, rounded to the nearest
// as Redis rounds it.
func (e *Engine) cmdTTL(args []string) []byte {
	if len(args) != 1 {
		return e.encode(wrongArguments("TTL"), false)
	}
	left := e.remainingTTL(args[0])
	e.noteLookup(left != -2)
	if left < 0 {
		return e.encode(left, false)
	}
	return e.encode((left+500)/1000, false)
}

// cmdPTTL is TTL in milliseconds.
func (e *Engine) cmdPTTL(args []string) []byte {
	if len(args) != 1 {
		return e.encode(wrongArguments("PTTL"), false)
	}
	left := e.remainingTTL(args[0])
	e.noteLookup(left != -2)
	return e.encode(left, false)
}

// cmdDEL removes keys from whichever keyspace holds them.
//
// It used to look only in the string dictionary, so DEL on a set, a sorted set,
// a filter or a sketch reported nothing deleted and deleted nothing - and where
// a name was held by two types at once it removed the string and left the rest.
// A delete that cannot delete is worse than a missing command, because it
// answers.
func (e *Engine) cmdDEL(args []string) []byte {
	if len(args) == 0 {
		return e.encode(wrongArguments("DEL"), false)
	}
	deleted := 0
	for _, key := range args {
		if e.space.DeleteAnywhere(key) {
			deleted++
		}
	}
	return e.encode(deleted, false)
}

// cmdUNLINK is DEL. Redis hands an unlinked value to a background thread to
// free; here deleting costs the same either way, so the two are one command,
// and UNLINK is logged as DEL - see persistedName.
func (e *Engine) cmdUNLINK(args []string) []byte {
	if len(args) == 0 {
		return e.encode(wrongArguments("UNLINK"), false)
	}
	return e.cmdDEL(args)
}

// cmdEXPIRE implements EXPIRE key seconds. A time already passed - zero or
// negative - deletes the key, as it does in Redis, and is logged as the DEL
// it amounts to.
func (e *Engine) cmdEXPIRE(args []string) []byte {
	return e.expireCommand("EXPIRE", args, 1000, false)
}

func (e *Engine) cmdPEXPIRE(args []string) []byte { return e.expireCommand("PEXPIRE", args, 1, false) }

func (e *Engine) cmdEXPIREAT(args []string) []byte {
	return e.expireCommand("EXPIREAT", args, 1000, true)
}

func (e *Engine) cmdPEXPIREAT(args []string) []byte {
	return e.expireCommand("PEXPIREAT", args, 1, true)
}

// expireCommand reads its options before its time, as Redis does, and refuses
// them in Redis's words.
func (e *Engine) expireCommand(name string, args []string, scale int64, absolute bool) []byte {
	if len(args) < 2 {
		return e.encode(wrongArguments(name), false)
	}
	nx, xx, gt, lt := false, false, false, false
	for _, opt := range args[2:] {
		switch strings.ToUpper(opt) {
		case "NX":
			nx = true
		case "XX":
			xx = true
		case "GT":
			gt = true
		case "LT":
			lt = true
		default:
			return e.encode(fmt.Errorf("ERR Unsupported option %s", EchoArgument(opt)), false)
		}
	}
	if nx && (xx || gt || lt) {
		return e.encode(errors.New("ERR NX and XX, GT or LT options at the same time are not compatible"), false)
	}
	if gt && lt {
		return e.encode(errors.New("ERR GT and LT options at the same time are not compatible"), false)
	}
	n, valid := e.counterInteger(args[1])
	if !valid {
		return e.encode(errNotAnInteger, false)
	}
	if n > math.MaxInt64/scale || n < math.MinInt64/scale {
		return e.encode(invalidExpireTime(name), false)
	}
	at := n * scale
	// One instant for the whole command, as Redis's expireGenericCommand uses
	// commandTimeSnapshot() both as the base of a relative time and in
	// checkAlreadyExpired (expire.c 652-661, 726-754). Reading the clock twice
	// let a slow moment between the reads delete a key PEXPIRE k 1 had just
	// given a future expiry, which Redis never does.
	now := e.commandClock()
	if !absolute && at > 0 {
		var ok bool
		at, ok = expiryInstantFrom(now, at)
		if !ok {
			return e.encode(invalidExpireTime(name), false)
		}
	}
	owner, ok := e.space.OwnerOf(args[0])
	if !ok {
		e.aof.skip = true
		return constant.RespZero
	}
	old, has := owner.GetExpiry(args[0])
	if (nx && has) || (xx && !has) || (gt && (!has || at <= int64(old))) || (lt && has && at >= int64(old)) {
		e.aof.skip = true
		return constant.RespZero
	}
	if at <= now && !e.aof.replaying && !e.replicaApplying {
		owner.Delete(args[0])
		e.aofRecord("DEL", args[0])
		return constant.RespOne
	}
	owner.SetExpiryAt(args[0], uint64(at))
	e.aofRecordInt("PEXPIREAT", args[0], at)
	return constant.RespOne
}

func (e *Engine) cmdPERSIST(args []string) []byte {
	if len(args) != 1 {
		return e.encode(wrongArguments("PERSIST"), false)
	}
	owner, ok := e.space.OwnerOf(args[0])
	if ok && owner.ClearExpiry(args[0]) {
		return constant.RespOne
	}
	return constant.RespZero
}

// cmdINCR, cmdDECR, cmdINCRBY and cmdDECRBY add to the integer a key holds;
// see opIncr. The increment is read before the key is looked at, as Redis
// reads it, so INCRBY of a list with a word for an increment is refused for
// the word.
func (e *Engine) cmdINCR(args []string) []byte {
	e.recordsItself()
	if len(args) != 1 {
		return e.encode(wrongArguments("INCR"), false)
	}
	return e.counterReply(e.opIncr(args[0]))
}

func (e *Engine) cmdDECR(args []string) []byte {
	e.recordsItself()
	if len(args) != 1 {
		return e.encode(wrongArguments("DECR"), false)
	}
	return e.counterReply(e.opDecr(args[0]))
}

func (e *Engine) cmdINCRBY(args []string) []byte {
	e.recordsItself()
	if len(args) != 2 {
		return e.encode(wrongArguments("INCRBY"), false)
	}
	by, valid := e.counterInteger(args[1])
	if !valid {
		return e.encode(errNotAnInteger, false)
	}
	return e.counterReply(e.opIncrBy(args[0], by))
}

func (e *Engine) cmdDECRBY(args []string) []byte {
	e.recordsItself()
	if len(args) != 2 {
		return e.encode(wrongArguments("DECRBY"), false)
	}
	by, valid := e.counterInteger(args[1])
	if !valid {
		return e.encode(errNotAnInteger, false)
	}
	return e.counterReply(e.opDecrBy(args[0], by))
}

// counterReply answers a counter's new value, or the refusal.
func (e *Engine) counterReply(n int64, err error) []byte {
	r := e.reply()
	if err != nil {
		return r.Error(err).Bytes()
	}
	return r.Int(n).Bytes()
}

// cmdDBSIZE counts the keys in every keyspace, not only the strings.
//
// It used to answer dictStore.Len(), which is the same bug the type checking
// fixed and this command was left out of: each type has its own store, and a
// command that knows about one of them reports on one of them. So SADD s m
// followed by DBSIZE answered 0 while the key was plainly there, and adding
// EXISTS and KEYS made the contradiction visible - KEYS * listing three keys
// next to a DBSIZE of zero.
func (e *Engine) cmdDBSIZE(args []string) []byte {
	if len(args) != 0 {
		return e.encode(wrongArguments("DBSIZE"), false)
	}
	return e.encode(int64(e.space.TotalKeys()), false)
}
