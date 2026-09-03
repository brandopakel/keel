package core

import (
	"errors"
	"strconv"

	"github.com/brandopakel/keel/internal/constant"
	"github.com/brandopakel/keel/internal/data_structure"
)

// List commands.
//
// A list, like a hash, has no empty form: popping the last element removes the
// key. Everything that can empty one goes through dropListIfEmpty, for the same
// reason - a key with no elements would answer EXISTS 1 and LRANGE nothing, and
// the rewrite would write it as an RPUSH with no values, which is a syntax
// error on replay.

func listFor(key string) (*data_structure.List, bool) {
	return listStore.Get(key)
}

func dropListIfEmpty(key string, l *data_structure.List) {
	if l.Len() == 0 {
		listStore.Delete(key)
		return
	}
	listStore.Resize(key)
}

// push is LPUSH and RPUSH, which differ only in the end they add to.
func push(args []string, front bool, name string) []byte {
	if len(args) < 2 {
		return Encode(errors.New("ERR wrong number of arguments for '"+name+"' command"), false)
	}
	key := args[0]

	l, ok := listFor(key)
	if !ok {
		l = data_structure.NewList()
		listStore.Put(key, l)
	}
	if front {
		l.PushFront(args[1:]...)
	} else {
		l.PushBack(args[1:]...)
	}
	listStore.Resize(key)
	return Encode(l.Len(), false)
}

func cmdLPUSH(args []string) []byte { return push(args, true, "LPUSH") }
func cmdRPUSH(args []string) []byte { return push(args, false, "RPUSH") }

// pop is LPOP and RPOP. Without a count it answers one element; with one it
// answers an array, which is Redis 6.2's behaviour and the one clients test
// for - a count of zero is an empty array rather than a nil.
func pop(args []string, front bool, name string) []byte {
	if len(args) < 1 || len(args) > 2 {
		return Encode(errors.New("ERR wrong number of arguments for '"+name+"' command"), false)
	}
	key := args[0]

	count := 1
	counted := len(args) == 2
	if counted {
		n, err := strconv.Atoi(args[1])
		if err != nil {
			return Encode(errors.New("ERR value is not an integer or out of range"), false)
		}
		if n < 0 {
			return Encode(errors.New("ERR value is out of range, must be positive"), false)
		}
		count = n
	}

	l, ok := listFor(key)
	if !ok {
		// A null array and a null bulk string are different replies, and which
		// one belongs here depends on what the command was going to answer.
		// Checked against Redis 8.10.1, which sends *-1 for the counted form
		// and $-1 for the bare one; sending $-1 for both is a type error in any
		// client that decodes the counted reply into a list.
		if counted {
			return constant.RespNilArray
		}
		return constant.RespNil
	}

	popped := make([]string, 0, count)
	for i := 0; i < count; i++ {
		var v string
		var got bool
		if front {
			v, got = l.PopFront()
		} else {
			v, got = l.PopBack()
		}
		if !got {
			break
		}
		popped = append(popped, v)
	}
	dropListIfEmpty(key, l)

	if !counted {
		if len(popped) == 0 {
			return constant.RespNil
		}
		return Encode(popped[0], false)
	}
	if len(popped) == 0 {
		return constant.RespEmptyArray
	}
	return Encode(popped, false)
}

func cmdLPOP(args []string) []byte { return pop(args, true, "LPOP") }
func cmdRPOP(args []string) []byte { return pop(args, false, "RPOP") }

func cmdLLEN(args []string) []byte {
	if len(args) != 1 {
		return Encode(errors.New("ERR wrong number of arguments for 'LLEN' command"), false)
	}
	l, ok := listFor(args[0])
	if !ok {
		return constant.RespZero
	}
	return Encode(l.Len(), false)
}

func cmdLINDEX(args []string) []byte {
	if len(args) != 2 {
		return Encode(errors.New("ERR wrong number of arguments for 'LINDEX' command"), false)
	}
	index, err := strconv.Atoi(args[1])
	if err != nil {
		return Encode(errors.New("ERR value is not an integer or out of range"), false)
	}

	l, ok := listFor(args[0])
	if !ok {
		return constant.RespNil
	}
	value, found := l.Index(index)
	if !found {
		return constant.RespNil
	}
	return Encode(value, false)
}

func cmdLSET(args []string) []byte {
	if len(args) != 3 {
		return Encode(errors.New("ERR wrong number of arguments for 'LSET' command"), false)
	}
	index, err := strconv.Atoi(args[1])
	if err != nil {
		return Encode(errors.New("ERR value is not an integer or out of range"), false)
	}

	l, ok := listFor(args[0])
	if !ok {
		return Encode(errors.New("ERR no such key"), false)
	}
	if !l.Set(index, args[2]) {
		return Encode(errors.New("ERR index out of range"), false)
	}
	listStore.Resize(args[0])
	return constant.RespOk
}

// cmdLRANGE answers the elements between two positions, both inclusive and both
// allowed to be negative or out of range - a range outside the list is empty
// rather than an error, which is what makes LRANGE key 0 -1 the idiom for
// "everything" whatever the length.
func cmdLRANGE(args []string) []byte {
	if len(args) != 3 {
		return Encode(errors.New("ERR wrong number of arguments for 'LRANGE' command"), false)
	}
	start, err := strconv.Atoi(args[1])
	if err != nil {
		return Encode(errors.New("ERR value is not an integer or out of range"), false)
	}
	stop, err := strconv.Atoi(args[2])
	if err != nil {
		return Encode(errors.New("ERR value is not an integer or out of range"), false)
	}

	l, ok := listFor(args[0])
	if !ok {
		return constant.RespEmptyArray
	}
	values := l.Range(start, stop)
	if len(values) == 0 {
		return constant.RespEmptyArray
	}
	return Encode(values, false)
}
