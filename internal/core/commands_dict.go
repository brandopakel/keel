package core

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/brandopakel/keel/internal/constant"
	"github.com/brandopakel/keel/internal/data_structure"
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
	if len(args) < 2 || len(args) == 3 || len(args) > 4 {
		return Encode(errors.New("(error) ERR wrong number of arguments for 'SET' command"), false)
	}

	var key, value string
	var ttlMs int64 = -1

	key, value = args[0], args[1]
	oType, oEnc := deduceTypeString(value)
	if len(args) > 2 {
		amount, err := strconv.ParseInt(args[3], 10, 64)
		if err != nil {
			return Encode(errors.New("(error) ERR value is not an integer or out of range"), false)
		}
		switch strings.ToUpper(args[2]) {
		case "EX":
			ttlMs = amount * 1000
		case "PX":
			ttlMs = amount
		default:
			return Encode(errors.New("(error) ERR syntax error"), false)
		}
		if amount <= 0 {
			// Redis's wording. A TTL already in the past is a delete dressed up
			// as a write, and answering OK to it would be the wrong answer to
			// two different questions at once.
			return Encode(errors.New("(error) ERR invalid expire time in 'set' command"), false)
		}
	}

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
		return Encode(errors.New("(error) ERR wrong number of arguments for 'GET' command"), false)
	}

	key := args[0]
	obj := dictStore.Get(key)
	if obj == nil {
		return constant.RespNil
	}

	if dictStore.HasExpired(key) {
		return constant.RespNil
	}

	return Encode(obj.Value, false)
}

func cmdTTL(args []string) []byte {
	if len(args) != 1 {
		return Encode(errors.New("(error) ERR wrong number of arguments for 'TTL' command"), false)
	}
	key := args[0]
	obj := dictStore.Get(key)
	if obj == nil {
		return constant.TtlKeyNotExist
	}

	exp, isExpirySet := dictStore.GetExpiry(key)
	if !isExpirySet {
		return constant.TtlKeyExistNoExpire
	}

	remainMs := exp - uint64(time.Now().UnixMilli())
	if remainMs < 0 {
		return constant.TtlKeyNotExist
	}

	return Encode(int64(remainMs/1000), false)
}

// cmdDEL removes keys from whichever keyspace holds them.
//
// It used to look only in the string dictionary, so DEL on a set, a sorted set,
// a filter or a sketch reported nothing deleted and deleted nothing - and where
// a name was held by two types at once it removed the string and left the rest.
// A delete that cannot delete is worse than a missing command, because it
// answers.
func cmdDEL(args []string) []byte {
	delCount := 0
	for _, key := range args {
		if data_structure.DeleteAnywhere(key) {
			delCount++
		}
	}
	return Encode(delCount, false)
}

func cmdEXPIRE(args []string) []byte {
	if len(args) < 2 {
		return Encode(errors.New("(error) ERR wrong number of arguments for 'EXPIRE' command"), false)
	}
	key := args[0]
	ttlSec, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return Encode(errors.New("(error) ERR value is not an integer or out of range"), false)
	}

	obj := dictStore.Get(key)
	if obj == nil {
		return constant.RespZero
	}

	dictStore.SetExpiry(key, ttlSec*1000)
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
		return Encode(errors.New("(error) ERR wrong number of arguments for 'PEXPIREAT' command"), false)
	}
	atMs, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return Encode(errors.New("(error) ERR value is not an integer or out of range"), false)
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

func cmdINCR(args []string) []byte {
	if len(args) != 1 {
		return Encode(errors.New("(error) ERR wrong number of arguments for 'INCR' command"), false)
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

	i, _ := strconv.ParseInt(obj.Value.(string), 10, 64)
	i++
	obj.Value = strconv.FormatInt(i, 10)

	return Encode(i, false)
}

// cmdDBSIZE reports how many keys the main dictionary holds.
//
// Without it, eviction is invisible from a client: the only way to observe it
// would be to guess which keys went away.
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
		return Encode(errors.New("(error) ERR wrong number of arguments for 'DBSIZE' command"), false)
	}
	return Encode(int64(data_structure.TotalKeys()), false)
}
