package core

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/brandopakel/keel/internal/constant"
	"github.com/brandopakel/keel/internal/data_structure"
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

// SETEX/PSETEX share SET's validation and canonical SET/PEXPIREAT persistence.
// Like SET, they replace a key whatever type held it.
func (e *Engine) cmdSETEX(args []string) []byte  { return e.setWithTTL("SETEX", args, "EX") }
func (e *Engine) cmdPSETEX(args []string) []byte { return e.setWithTTL("PSETEX", args, "PX") }

// cmdSETNX is SET key value NX answering 1 or 0, the older spelling clients
// still send: cachelib's and Flask-Caching's add() are built on it. It goes
// through SET, so it is logged as the SET it performs and a log written here
// replays on a build that predates the name.
func (e *Engine) cmdSETNX(args []string) []byte {
	if len(args) != 2 {
		return Encode(wrongArguments("SETNX"), false)
	}
	switch reply := e.cmdSET([]string{args[0], args[1], "NX"}); {
	case bytes.Equal(reply, constant.RespOk):
		return constant.RespOne
	case bytes.Equal(reply, nullReply()):
		return constant.RespZero
	default:
		return reply
	}
}

func (e *Engine) setWithTTL(name string, args []string, unit string) []byte {
	if len(args) != 3 {
		return Encode(wrongArguments(name), false)
	}
	return e.setCommand(name, []string{args[0], args[2], unit, args[1]})
}

// cmdSET implements SET key value [EX seconds | PX milliseconds].
//
// The expiry keyword used to be skipped over entirely: whatever sat in args[2]
// was ignored and args[3] was read as a number of seconds. So PX asked for
// milliseconds and got seconds, a thousand times longer than the caller wanted
// and silently - the reply was still OK - and a keyword that meant nothing at
// all was accepted just as readily. Anything past EX and PX is refused rather
// than guessed at, which is the difference between a command this server does
// not implement and a command it appears to implement and does not.
func (e *Engine) cmdSET(args []string) []byte { return e.setCommand("SET", args) }

// setCommand is SET, and SETEX and PSETEX through it; name is the command
// running, which an invalid expiry names. The options are read as Redis reads
// them: NX and XX exclude each other, KEEPTTL and the four expiry options
// exclude one another, though one expiry option may be given again, and its
// last value counts. The expiry is checked only once every option has been
// read, so a malformed option is refused ahead of a malformed expiry.
func (e *Engine) setCommand(name string, args []string) []byte {
	if len(args) < 2 {
		return Encode(wrongArguments(name), false)
	}
	nx, xx, get, keep := false, false, false, false
	unit, expire := "", ""
	for i := 2; i < len(args); i++ {
		switch opt := strings.ToUpper(args[i]); {
		case opt == "NX" && !xx:
			nx = true
		case opt == "XX" && !nx:
			xx = true
		case opt == "GET":
			get = true
		case opt == "KEEPTTL" && unit == "":
			keep = true
		case (opt == "EX" || opt == "PX" || opt == "EXAT" || opt == "PXAT") && !keep &&
			(unit == "" || unit == opt) && i+1 < len(args):
			unit, expire = opt, args[i+1]
			i++
		default:
			return Encode(errSyntax, false)
		}
	}
	var at int64
	expiry := unit != ""
	if expiry {
		n, valid := counterInteger(expire)
		if !valid {
			return Encode(errNotAnInteger, false)
		}
		if n <= 0 {
			return Encode(invalidExpireTime(name), false)
		}
		if unit == "EX" || unit == "EXAT" {
			if n > math.MaxInt64/1000 {
				return Encode(invalidExpireTime(name), false)
			}
			n *= 1000
		}
		at = n
		if unit == "EX" || unit == "PX" {
			var ok bool
			at, ok = expiryInstant(n)
			if !ok {
				return Encode(invalidExpireTime(name), false)
			}
		}
	}
	key, value := args[0], args[1]
	// A name another type holds is a key that exists, so NX leaves it and XX
	// replaces it, and GET cannot read it as a string - Redis's rules, see
	// replacingWrites.
	other, otherHeld := e.space.OwnerOf(key)
	otherHeld = otherHeld && other.KeyspaceName() != e.dictStore.KeyspaceName()
	if otherHeld && get {
		return Encode(errWrongType, false)
	}
	obj := e.dictStore.Get(key)
	reply := constant.RespOk
	if get {
		reply = nullReply()
		if obj != nil {
			reply = encodeBoundedString(obj.Value)
			if len(reply) > 0 && reply[0] == '-' {
				return reply
			}
		}
	}
	exists := obj != nil || otherHeld
	if (nx && exists) || (xx && !exists) {
		aof.skip = true
		if get {
			return reply
		}
		return nullReply()
	}
	if keep && exists {
		// KEEPTTL keeps the key's expiry, and the key is the name: a hash
		// replaced by a string under KEEPTTL keeps the hash's deadline.
		var old uint64
		var has bool
		if obj != nil {
			old, has = e.dictStore.GetExpiry(key)
		} else {
			old, has = other.GetExpiry(key)
		}
		if has {
			at, expiry = int64(old), true
		}
	}
	if otherHeld {
		dropOtherType(other, key)
	}
	e.dictStore.Put(key, e.dictStore.NewObj(value))
	aofRecord("SET", key, value)
	if expiry {
		e.dictStore.SetExpiryAt(key, uint64(at))
		aofRecord("PEXPIREAT", key, strconv.FormatInt(at, 10))
	}
	return reply
}

// dropOtherType deletes a key another type holds, for a write that replaces
// it, and stages the DEL that amounts to ahead of the write's own record.
//
// Replay would reach the same state without the DEL, because it runs the same
// SET. It is logged anyway for the build before this one: there SET over a
// hash answers WRONGTYPE, a replay command that fails stops startup, and so a
// rollback could not read a log that relied on SET replacing it.
func dropOtherType(owner data_structure.Keyspace, key string) {
	owner.Delete(key)
	aofRecord("DEL", key)
}

// expiryInstant turns a positive duration in milliseconds into the instant it
// ends, reading the clock once, and reports false when that instant does not
// fit in the signed 64 bits it is kept and compared in.
func expiryInstant(ttlMs int64) (int64, bool) {
	now := time.Now().UnixMilli()
	if ttlMs > math.MaxInt64-now {
		return 0, false
	}
	return now + ttlMs, true
}

func (e *Engine) cmdGET(args []string) []byte {
	if len(args) != 1 {
		return Encode(wrongArguments("GET"), false)
	}
	// Get reaps a key whose TTL has passed, so what comes back is live.
	obj := e.dictStore.Get(args[0])
	if obj == nil {
		return nullReply()
	}
	return encodeBoundedString(obj.Value)
}

// remainingTTL is how long a key has left, in milliseconds. The two negative
// answers are Redis's: -2 for a key that is not there, -1 for one with no
// expiry.
func remainingTTL(key string) int64 {
	owner, ok := data_structure.OwnerOf(key)
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
func cmdTTL(args []string) []byte {
	if len(args) != 1 {
		return Encode(wrongArguments("TTL"), false)
	}
	left := remainingTTL(args[0])
	if left < 0 {
		return Encode(left, false)
	}
	return Encode((left+500)/1000, false)
}

// cmdPTTL is TTL in milliseconds.
func cmdPTTL(args []string) []byte {
	if len(args) != 1 {
		return Encode(wrongArguments("PTTL"), false)
	}
	return Encode(remainingTTL(args[0]), false)
}

// cmdDEL removes keys from whichever keyspace holds them.
//
// It used to look only in the string dictionary, so DEL on a set, a sorted set,
// a filter or a sketch reported nothing deleted and deleted nothing - and where
// a name was held by two types at once it removed the string and left the rest.
// A delete that cannot delete is worse than a missing command, because it
// answers.
func cmdDEL(args []string) []byte {
	if len(args) == 0 {
		return Encode(wrongArguments("DEL"), false)
	}
	deleted := 0
	for _, key := range args {
		if data_structure.DeleteAnywhere(key) {
			deleted++
		}
	}
	return Encode(deleted, false)
}

// cmdUNLINK is DEL. Redis hands an unlinked value to a background thread to
// free; here deleting costs the same either way, so the two are one command,
// and UNLINK is logged as DEL - see persistedName.
func cmdUNLINK(args []string) []byte {
	if len(args) == 0 {
		return Encode(wrongArguments("UNLINK"), false)
	}
	return cmdDEL(args)
}

// cmdEXPIRE implements EXPIRE key seconds. A time already passed - zero or
// negative - deletes the key, as it does in Redis, and is logged as the DEL
// it amounts to.
func cmdEXPIRE(args []string) []byte    { return expireCommand("EXPIRE", args, 1000, false) }
func cmdPEXPIRE(args []string) []byte   { return expireCommand("PEXPIRE", args, 1, false) }
func cmdEXPIREAT(args []string) []byte  { return expireCommand("EXPIREAT", args, 1000, true) }
func cmdPEXPIREAT(args []string) []byte { return expireCommand("PEXPIREAT", args, 1, true) }

// expireCommand reads its options before its time, as Redis does, and refuses
// them in Redis's words.
func expireCommand(name string, args []string, scale int64, absolute bool) []byte {
	if len(args) < 2 {
		return Encode(wrongArguments(name), false)
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
			return Encode(fmt.Errorf("ERR Unsupported option %s", EchoArgument(opt)), false)
		}
	}
	if nx && (xx || gt || lt) {
		return Encode(errors.New("ERR NX and XX, GT or LT options at the same time are not compatible"), false)
	}
	if gt && lt {
		return Encode(errors.New("ERR GT and LT options at the same time are not compatible"), false)
	}
	n, valid := counterInteger(args[1])
	if !valid {
		return Encode(errNotAnInteger, false)
	}
	if n > math.MaxInt64/scale || n < math.MinInt64/scale {
		return Encode(invalidExpireTime(name), false)
	}
	at := n * scale
	if !absolute && at > 0 {
		var ok bool
		at, ok = expiryInstant(at)
		if !ok {
			return Encode(invalidExpireTime(name), false)
		}
	}
	owner, ok := data_structure.OwnerOf(args[0])
	if !ok {
		aof.skip = true
		return constant.RespZero
	}
	old, has := owner.GetExpiry(args[0])
	if (nx && has) || (xx && !has) || (gt && (!has || at <= int64(old))) || (lt && has && at >= int64(old)) {
		aof.skip = true
		return constant.RespZero
	}
	if at <= time.Now().UnixMilli() && !aof.replaying && !replicaApplying {
		owner.Delete(args[0])
		aofRecord("DEL", args[0])
		return constant.RespOne
	}
	owner.SetExpiryAt(args[0], uint64(at))
	aofRecord("PEXPIREAT", args[0], strconv.FormatInt(at, 10))
	return constant.RespOne
}

func cmdPERSIST(args []string) []byte {
	if len(args) != 1 {
		return Encode(wrongArguments("PERSIST"), false)
	}
	owner, ok := data_structure.OwnerOf(args[0])
	if ok && owner.ClearExpiry(args[0]) {
		return constant.RespOne
	}
	return constant.RespZero
}

// cmdINCR adds one to the integer a key holds, treating a missing key as zero.
// The value is changed in place, so a TTL on the key survives, as it does in
// Redis. A value that is not a canonical integer is refused, and so is one
// that would overflow, rather than wrapping to a number nobody asked for.
func (e *Engine) cmdINCR(args []string) []byte   { return e.increment("INCR", args, 1, false) }
func (e *Engine) cmdDECR(args []string) []byte   { return e.increment("DECR", args, -1, false) }
func (e *Engine) cmdINCRBY(args []string) []byte { return e.increment("INCRBY", args, 1, true) }
func (e *Engine) cmdDECRBY(args []string) []byte { return e.increment("DECRBY", args, -1, true) }
func (e *Engine) increment(name string, args []string, sign int64, explicit bool) []byte {
	want := 1
	if explicit {
		want = 2
	}
	if len(args) != want {
		return Encode(wrongArguments(name), false)
	}
	delta := sign
	if explicit {
		n, valid := counterInteger(args[1])
		if !valid {
			return Encode(errNotAnInteger, false)
		}
		if sign == -1 && n == math.MinInt64 {
			// Negating it would overflow before anything is added.
			return Encode(errDecrOverflow, false)
		}
		delta = n * sign
	}
	key := args[0]
	obj := e.dictStore.Get(key)
	current := int64(0)
	if obj != nil {
		var valid bool
		current, valid = canonicalInteger(obj.Value)
		if !valid {
			return Encode(errNotAnInteger, false)
		}
	}
	if (delta > 0 && current > math.MaxInt64-delta) || (delta < 0 && current < math.MinInt64-delta) {
		return Encode(errIncrOverflow, false)
	}
	current += delta
	value := strconv.FormatInt(current, 10)
	if obj == nil {
		e.dictStore.Put(key, e.dictStore.NewObj(value))
	} else {
		e.dictStore.UpdateValue(key, value)
	}
	return Encode(current, false)
}

// cmdDBSIZE counts the keys in every keyspace, not only the strings.
//
// It used to answer dictStore.Len(), which is the same bug the type checking
// fixed and this command was left out of: each type has its own store, and a
// command that knows about one of them reports on one of them. So SADD s m
// followed by DBSIZE answered 0 while the key was plainly there, and adding
// EXISTS and KEYS made the contradiction visible - KEYS * listing three keys
// next to a DBSIZE of zero.
func cmdDBSIZE(args []string) []byte {
	if len(args) != 0 {
		return Encode(wrongArguments("DBSIZE"), false)
	}
	return Encode(int64(data_structure.TotalKeys()), false)
}
