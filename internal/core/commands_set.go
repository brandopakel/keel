package core

import (
	"errors"
	"strconv"

	"github.com/brandopakel/keel/internal/constant"
	"github.com/brandopakel/keel/internal/data_structure"
)

func cmdSADD(args []string) []byte {
	if len(args) < 2 {
		return Encode(errors.New("(error) ERR wrong number of arguments for 'SADD' command"), false)
	}
	key := args[0] // TODO: check key is used by other types or not
	set, exist := setStore.Get(key)
	if !exist {
		set = data_structure.CreateSet(key)
		setStore.Put(key, set)
	}
	// Members change the set's size without going through Put, so the keyspace
	// has to re-measure it or the memory budget keeps believing the old figure.
	defer setStore.Resize(key)
	count := set.Add(args[1:]...)
	return Encode(count, false)
}

func cmdSREM(args []string) []byte {
	if len(args) < 2 {
		return Encode(errors.New("(error) ERR wrong number of arguments for 'SADD' command"), false)
	}
	key := args[0]
	set, exist := setStore.Get(key)
	if !exist {
		set = data_structure.CreateSet(key)
		setStore.Put(key, set)
	}
	// Members change the set's size without going through Put, so the keyspace
	// has to re-measure it or the memory budget keeps believing the old figure.
	defer setStore.Resize(key)
	count := set.Rem(args[1:]...)
	return Encode(count, false)
}

func cmdSCARD(args []string) []byte {
	if len(args) != 1 {
		return Encode(errors.New("(error) ERR wrong number of arguments for 'SCARD' command"), false)
	}
	key := args[0]
	set, exist := setStore.Get(key)
	if !exist {
		return Encode(0, false)
	}
	return Encode(set.Size(), false)
}

func cmdSMEMBERS(args []string) []byte {
	if len(args) != 1 {
		return Encode(errors.New("(error) ERR wrong number of arguments for 'SMEMBERS' command"), false)
	}
	key := args[0]
	set, exist := setStore.Get(key)
	if !exist {
		return Encode(make([]string, 0), false)
	}
	return Encode(set.Members(), false)
}

func cmdSISMEMBER(args []string) []byte {
	if len(args) != 2 {
		return Encode(errors.New("(error) ERR wrong number of arguments for 'SISMEMBER' command"), false)
	}
	key := args[0]
	set, exist := setStore.Get(key)
	if !exist {
		return Encode(0, false)
	}
	return Encode(set.IsMember(args[1]), false)
}

func cmdSMISMEMBER(args []string) []byte {
	if len(args) < 2 {
		return Encode(errors.New("(error) ERR wrong number of arguments for 'SMISMEMBER' command"), false)
	}
	key := args[0]
	set, exist := setStore.Get(key)
	if !exist {
		res := make([]int, len(args)-1)
		return Encode(res, false)
	}
	return Encode(set.MIsMember(args[1:]...), false)
}

func cmdSPOP(args []string) []byte {
	if len(args) > 2 {
		return Encode(errors.New("(error) ERR wrong number of arguments for 'SPOP' command"), false)
	}
	key := args[0]
	hasCount := len(args) > 1
	count := 0
	if hasCount {
		n, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			return Encode(errors.New("(error) Count must be int"), false)
		}
		count = int(n)
	}

	set, exist := setStore.Get(key)
	if !exist {
		if !hasCount {
			return Encode(nil, false)
		}
		return Encode(make([]string, 0), false)
	}
	// SPOP removes members, so the set shrinks.
	defer setStore.Resize(key)

	// Without a count the command pops one member, not zero. Asking for zero
	// and then reading the first of the nothing that came back is what made
	// plain SPOP panic, and a panic here is the whole server rather than the
	// connection that sent it.
	want := count
	if !hasCount {
		want = 1
	}
	popped := set.Pop(want)

	// Replaying SPOP would pop different members, so the log records the
	// removal that actually happened rather than the request that caused it.
	if len(popped) > 0 {
		aofRecord(append([]string{"SREM", key}, popped...)...)
	}
	if !hasCount {
		if len(popped) == 0 {
			return constant.RespNil
		}
		return Encode(popped[0], false)
	}
	return Encode(popped, false)
}

func cmdSRAND(args []string) []byte {
	if len(args) > 2 {
		return Encode(errors.New("(error) ERR wrong number of arguments for 'SRAND' command"), false)
	}
	key := args[0]
	hasCount := len(args) > 1
	count := 0
	if hasCount {
		n, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			return Encode(errors.New("(error) Count must be int"), false)
		}
		count = int(n)
	}

	set, exist := setStore.Get(key)
	if !exist {
		if !hasCount {
			return Encode(nil, false)
		}
		return Encode(make([]string, 0), false)
	}
	if !hasCount {
		// One member, for the same reason SPOP takes one.
		picked := set.Rand(1)
		if len(picked) == 0 {
			return constant.RespNil
		}
		return Encode(picked[0], false)
	}
	return Encode(set.Rand(count), false)
}
