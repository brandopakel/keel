package core

import (
	"errors"
	"memkv/internal/constant"
	"strconv"
	"time"
)

func cmdSET(args []string) []byte {
	if len(args) < 2 || len(args) == 3 || len(args) > 4 {
		return Encode(errors.New("(error) ERR wrong number of arguments for 'SET' command"), false)
	}

	var key, value string
	var ttlMs int64 = -1

	key, value = args[0], args[1]
	oType, oEnc := deduceTypeString(value)
	if len(args) > 2 {
		ttlSec, err := strconv.ParseInt(args[3], 10, 64)
		if err != nil {
			return Encode(errors.New("(error) ERR value is not an integer or out of range"), false)
		}
		ttlMs = ttlSec * 1000
	}

	dictStore.Put(key, dictStore.NewObj(value, ttlMs, oType, oEnc))
	if ttlMs > 0 {
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

	if dictStore.HasExpired(obj) {
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

	exp, isExpirySet := dictStore.GetExpiry(obj)
	if !isExpirySet {
		return constant.TtlKeyExistNoExpire
	}

	remainMs := exp - uint64(time.Now().UnixMilli())
	if remainMs < 0 {
		return constant.TtlKeyNotExist
	}

	return Encode(int64(remainMs/1000), false)
}

func cmdDEL(args []string) []byte {
	delCount := 0

	for _, key := range args {
		if ok := dictStore.Del(key); ok {
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

	dictStore.SetExpiry(obj, ttlSec*1000)
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
	dictStore.SetExpiryAt(obj, uint64(atMs))
	return constant.RespOne
}

func cmdINCR(args []string) []byte {
	if len(args) != 1 {
		return Encode(errors.New("(error) ERR wrong number of arguments for 'INCR' command"), false)
	}
	key := args[0]
	obj := dictStore.Get(key)
	if obj == nil {
		obj = dictStore.NewObj("0", constant.NoExpire, constant.ObjTypeString, constant.ObjEncodingInt)
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
func cmdDBSIZE(args []string) []byte {
	if len(args) != 0 {
		return Encode(errors.New("(error) ERR wrong number of arguments for 'DBSIZE' command"), false)
	}
	return Encode(int64(dictStore.Len()), false)
}
