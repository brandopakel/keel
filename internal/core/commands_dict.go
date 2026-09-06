package core

import (
	"errors"
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
	errSetExpire    = errors.New("ERR invalid expire time in 'set' command")
	errExpireExpire = errors.New("ERR invalid expire time in 'expire' command")
	errIncrOverflow = errors.New("ERR increment or decrement would overflow")
)

// cmdSET implements SET key value [EX seconds | PX milliseconds].
//
// The expiry keyword used to be skipped over entirely: whatever sat in args[2]
// was ignored and args[3] was read as a number of seconds. So PX asked for
// milliseconds and got seconds, a thousand times longer than the caller wanted
// and silently - the reply was still OK - and a keyword that meant nothing at
// all was accepted just as readily. Anything past EX and PX is refused rather
// than guessed at, which is the difference between a command this server does
// not implement and a command it appears to implement and does not.
func cmdSET(args []string) []byte {
	if len(args) < 2 {
		return Encode(errors.New("ERR wrong number of arguments for 'SET' command"), false)
	}
	nx, xx, get, keep := false, false, false, false
	var at int64
	expiry := false
	for i := 2; i < len(args); i++ {
		switch strings.ToUpper(args[i]) {
		case "NX":
			nx = true
		case "XX":
			xx = true
		case "GET":
			get = true
		case "KEEPTTL":
			if expiry {
				return Encode(errSyntax, false)
			}
			keep = true
		case "EX", "PX", "EXAT", "PXAT":
			if expiry || keep || i+1 == len(args) {
				return Encode(errSyntax, false)
			}
			opt := strings.ToUpper(args[i])
			i++
			n, err := strconv.ParseInt(args[i], 10, 64)
			if err != nil {
				return Encode(errNotAnInteger, false)
			}
			if n <= 0 {
				return Encode(errSetExpire, false)
			}
			if opt == "EX" || opt == "EXAT" {
				if n > math.MaxInt64/1000 {
					return Encode(errSetExpire, false)
				}
				n *= 1000
			}
			at = n
			if opt == "EX" || opt == "PX" {
				var ok bool
				at, ok = expiryInstant(n)
				if !ok {
					return Encode(errSetExpire, false)
				}
			}
			expiry = true
		default:
			return Encode(errSyntax, false)
		}
	}
	if nx && xx {
		return Encode(errSyntax, false)
	}
	key, value := args[0], args[1]
	obj := dictStore.Get(key)
	reply := constant.RespOk
	if get {
		reply = constant.RespNil
		if obj != nil {
			reply = Encode(obj.Value, false)
		}
	}
	if (nx && obj != nil) || (xx && obj == nil) {
		aof.skip = true
		if get {
			return reply
		}
		return constant.RespNil
	}
	if keep && obj != nil {
		old, has := dictStore.GetExpiry(key)
		if has {
			at, expiry = int64(old), true
		}
	}
	t, enc := deduceTypeString(value)
	dictStore.Put(key, dictStore.NewObj(value, t, enc))
	aofRecord("SET", key, value)
	if expiry {
		dictStore.SetExpiryAt(key, uint64(at))
		aofRecord("PEXPIREAT", key, strconv.FormatInt(at, 10))
	}
	return reply
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

func cmdGET(args []string) []byte {
	if len(args) != 1 {
		return Encode(errors.New("ERR wrong number of arguments for 'GET' command"), false)
	}
	// Get reaps a key whose TTL has passed, so what comes back is live.
	obj := dictStore.Get(args[0])
	if obj == nil {
		return constant.RespNil
	}
	return Encode(obj.Value, false)
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
		return Encode(errors.New("ERR wrong number of arguments for 'TTL' command"), false)
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
		return Encode(errors.New("ERR wrong number of arguments for 'PTTL' command"), false)
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
		return Encode(errors.New("ERR wrong number of arguments for 'DEL' command"), false)
	}
	deleted := 0
	for _, key := range args {
		if data_structure.DeleteAnywhere(key) {
			deleted++
		}
	}
	return Encode(deleted, false)
}

// cmdEXPIRE implements EXPIRE key seconds. A time already passed - zero or
// negative - deletes the key, as it does in Redis, and is logged as the DEL
// it amounts to.
func cmdEXPIRE(args []string) []byte    { return expireCommand(args, 1000, false) }
func cmdPEXPIRE(args []string) []byte   { return expireCommand(args, 1, false) }
func cmdEXPIREAT(args []string) []byte  { return expireCommand(args, 1000, true) }
func cmdPEXPIREAT(args []string) []byte { return expireCommand(args, 1, true) }

func expireCommand(args []string, scale int64, absolute bool) []byte {
	if len(args) < 2 {
		return Encode(errors.New("ERR wrong number of arguments for expiry command"), false)
	}
	n, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return Encode(errNotAnInteger, false)
	}
	if n > math.MaxInt64/scale || n < math.MinInt64/scale {
		return Encode(errExpireExpire, false)
	}
	at := n * scale
	if !absolute && at > 0 {
		var ok bool
		at, ok = expiryInstant(at)
		if !ok {
			return Encode(errExpireExpire, false)
		}
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
			return Encode(errSyntax, false)
		}
	}
	if (nx && (xx || gt || lt)) || (gt && lt) {
		return Encode(errSyntax, false)
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
	if at <= time.Now().UnixMilli() && !aof.replaying {
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
		return Encode(errSyntax, false)
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
func cmdINCR(args []string) []byte   { return increment(args, 1, false) }
func cmdDECR(args []string) []byte   { return increment(args, -1, false) }
func cmdINCRBY(args []string) []byte { return increment(args, 1, true) }
func cmdDECRBY(args []string) []byte { return increment(args, -1, true) }
func increment(args []string, sign int64, explicit bool) []byte {
	want := 1
	if explicit {
		want = 2
	}
	if len(args) != want {
		return Encode(errors.New("ERR wrong number of arguments for increment command"), false)
	}
	delta := sign
	if explicit {
		n, valid := counterInteger(args[1])
		if !valid {
			return Encode(errNotAnInteger, false)
		}
		if sign == -1 && n == math.MinInt64 {
			return Encode(errIncrOverflow, false)
		}
		delta = n * sign
	}
	key := args[0]
	obj := dictStore.Get(key)
	current := int64(0)
	if obj != nil {
		var valid bool
		current, valid = canonicalInteger(obj.Value.(string))
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
		dictStore.Put(key, dictStore.NewObj(value, constant.ObjTypeString, constant.ObjEncodingInt))
	} else {
		dictStore.UpdateValue(key, value)
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
		return Encode(errors.New("ERR wrong number of arguments for 'DBSIZE' command"), false)
	}
	return Encode(int64(data_structure.TotalKeys()), false)
}
