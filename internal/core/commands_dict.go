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
	key, value := args[0], args[1]

	var ttlMs int64
	switch len(args) {
	case 2:
	case 4:
		amount, err := strconv.ParseInt(args[3], 10, 64)
		if err != nil {
			return Encode(errNotAnInteger, false)
		}
		switch strings.ToUpper(args[2]) {
		case "EX":
			if amount > math.MaxInt64/1000 {
				return Encode(errSetExpire, false)
			}
			ttlMs = amount * 1000
		case "PX":
			ttlMs = amount
		default:
			return Encode(errSyntax, false)
		}
		if amount <= 0 {
			// Redis's wording. A TTL already in the past is a delete dressed up
			// as a write, and answering OK to it would be the wrong answer to
			// two different questions at once.
			return Encode(errSetExpire, false)
		}
		// The expiry is stored as an instant, so the duration is bounded by
		// how far the clock can go: a TTL that would carry the instant past
		// the largest signed 64-bit value wraps when it is read back, and
		// a key that never expires is the wrong way to fail.
		if ttlMs > math.MaxInt64-time.Now().UnixMilli() {
			return Encode(errSetExpire, false)
		}
	default:
		return Encode(errSyntax, false)
	}

	oType, oEnc := deduceTypeString(value)
	dictStore.Put(key, dictStore.NewObj(value, oType, oEnc))
	if ttlMs > 0 {
		// After the Put, which clears whatever expiry the key had before.
		dictStore.SetExpiry(key, ttlMs)
		// The TTL arrived as a duration and has to be logged as an instant, so
		// the value and its expiry are recorded as two commands.
		aofRecord("SET", key, value)
		aofExpireAt(key)
	}
	return constant.RespOk
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
	if dictStore.Get(key) == nil {
		return -2
	}
	at, has := dictStore.GetExpiry(key)
	if !has {
		return -1
	}
	left := int64(at) - time.Now().UnixMilli()
	if left < 0 {
		return 0
	}
	return left
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
func cmdEXPIRE(args []string) []byte {
	if len(args) != 2 {
		return Encode(errors.New("ERR wrong number of arguments for 'EXPIRE' command"), false)
	}
	key := args[0]
	seconds, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return Encode(errNotAnInteger, false)
	}
	if dictStore.Get(key) == nil {
		return constant.RespZero
	}
	if seconds <= 0 {
		dictStore.Del(key)
		aofRecord("DEL", key)
		return constant.RespOne
	}
	if seconds > (math.MaxInt64-time.Now().UnixMilli())/1000 {
		return Encode(errExpireExpire, false)
	}
	dictStore.SetExpiry(key, seconds*1000)
	aofExpireAt(key)
	return constant.RespOne
}

// cmdPEXPIREAT sets a key's expiry to an absolute time in milliseconds.
//
// EXPIRE says "in ten seconds", which is a different instant every time it is
// evaluated. That is fine from a client and wrong in a log: replaying it a day
// later grants a fresh ten seconds, so every restart silently renews every TTL
// in the keyspace and nothing with an expiry ever actually expires. The
// append-only file therefore records this instead, which names an instant that
// does not move. Redis rewrites EXPIRE the same way and for the same reason.
func cmdPEXPIREAT(args []string) []byte {
	if len(args) != 2 {
		return Encode(errors.New("ERR wrong number of arguments for 'PEXPIREAT' command"), false)
	}
	atMs, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return Encode(errNotAnInteger, false)
	}

	obj := dictStore.Get(args[0])
	if obj == nil {
		return constant.RespZero
	}
	if atMs <= time.Now().UnixMilli() {
		// Already in the past, so this is a delete. Saying so now beats
		// storing an expiry that the next read would act on anyway.
		dictStore.Del(args[0])
		return constant.RespOne
	}
	dictStore.SetExpiryAt(args[0], uint64(atMs))
	return constant.RespOne
}

// cmdINCR adds one to the integer a key holds, treating a missing key as zero.
// The value is changed in place, so a TTL on the key survives, as it does in
// Redis. A value that is not a canonical integer is refused, and so is one
// that would overflow, rather than wrapping to a number nobody asked for.
func cmdINCR(args []string) []byte {
	if len(args) != 1 {
		return Encode(errors.New("ERR wrong number of arguments for 'INCR' command"), false)
	}
	key := args[0]
	obj := dictStore.Get(key)
	if obj == nil {
		obj = dictStore.NewObj("0", constant.ObjTypeString, constant.ObjEncodingInt)
		dictStore.Put(key, obj)
	}
	if err := assertType(obj.TypeEncoding, constant.ObjTypeString); err != nil {
		return Encode(err, false)
	}
	if err := assertEncoding(obj.TypeEncoding, constant.ObjEncodingInt); err != nil {
		return Encode(err, false)
	}

	current, err := strconv.ParseInt(obj.Value.(string), 10, 64)
	if err != nil {
		return Encode(errNotAnInteger, false)
	}
	if current == math.MaxInt64 {
		return Encode(errIncrOverflow, false)
	}
	current++
	obj.Value = strconv.FormatInt(current, 10)
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
