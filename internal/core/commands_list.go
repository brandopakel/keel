package core

import (
	"errors"

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

func (e *Engine) listFor(key string) (*data_structure.List, bool) {
	return e.listStore.Get(key)
}

func (e *Engine) dropListIfEmpty(key string, l *data_structure.List) {
	if l.Len() == 0 {
		e.listStore.Delete(key)
		return
	}
	e.listStore.Resize(key)
}

// push is LPUSH and RPUSH, which differ only in the end they add to.
func (e *Engine) push(args []string, front bool, name string) []byte {
	if len(args) < 2 {
		return e.encode(wrongArguments(name), false)
	}
	key := args[0]

	l, ok := e.listFor(key)
	if !ok {
		l = data_structure.NewList()
		e.listStore.Put(key, l)
	}
	if front {
		l.PushFront(args[1:]...)
	} else {
		l.PushBack(args[1:]...)
	}
	e.listStore.Resize(key)
	return e.encode(l.Len(), false)
}

func (e *Engine) cmdLPUSH(args []string) []byte { return e.push(args, true, "LPUSH") }
func (e *Engine) cmdRPUSH(args []string) []byte { return e.push(args, false, "RPUSH") }

// pop is LPOP and RPOP. Without a count it answers one element; with one it
// answers an array, which is Redis 6.2's behaviour and the one clients test
// for - a count of zero is an empty array rather than a nil.
func (e *Engine) pop(args []string, front bool, name string) []byte {
	if len(args) < 1 || len(args) > 2 {
		return e.encode(wrongArguments(name), false)
	}
	key := args[0]

	count := 1
	counted := len(args) == 2
	if counted {
		n, err := positiveCount(args[1])
		if err != nil {
			return e.encode(err, false)
		}
		count = int(n)
	}

	l, ok := e.listFor(key)
	if !ok {
		// A null array and a null bulk string are different replies, and which
		// one belongs here depends on what the command was going to answer.
		// Checked against Redis 8.10.1, which sends *-1 for the counted form
		// and $-1 for the bare one; sending $-1 for both is a type error in any
		// client that decodes the counted reply into a list. RESP3 has one
		// null for both.
		if counted {
			return e.nullArrayReply()
		}
		return e.nullReply()
	}

	count = min(count, l.Len())
	walk := replyWalk(func(yield func(string) bool) {
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
	})
	shape := shapeArray
	if !counted {
		shape = shapeOne
	}
	out := e.encodeWalkReply(walk, shape)
	if len(out) > 0 && out[0] == '-' {
		return out
	}
	for i := 0; i < count; i++ {
		if front {
			l.PopFront()
		} else {
			l.PopBack()
		}
	}
	e.dropListIfEmpty(key, l)
	return out
}

func (e *Engine) cmdLPOP(args []string) []byte { return e.pop(args, true, "LPOP") }
func (e *Engine) cmdRPOP(args []string) []byte { return e.pop(args, false, "RPOP") }

func (e *Engine) cmdLLEN(args []string) []byte {
	if len(args) != 1 {
		return e.encode(wrongArguments("LLEN"), false)
	}
	l, ok := e.listFor(args[0])
	if !ok {
		return constant.RespZero
	}
	return e.encode(l.Len(), false)
}

// cmdLINDEX looks the key up before it reads the index, as Redis does, so a
// key that is not there answers nil whatever the index says.
func (e *Engine) cmdLINDEX(args []string) []byte {
	if len(args) != 2 {
		return e.encode(wrongArguments("LINDEX"), false)
	}
	l, ok := e.listFor(args[0])
	if !ok {
		return e.nullReply()
	}
	index, valid := counterInteger(args[1])
	if !valid {
		return e.encode(errNotAnInteger, false)
	}
	value, found := l.Index(int(index))
	if !found {
		return e.nullReply()
	}
	return e.encodeBoundedString(value)
}

// cmdLSET, like LINDEX, finds the key before it reads the index.
func (e *Engine) cmdLSET(args []string) []byte {
	if len(args) != 3 {
		return e.encode(wrongArguments("LSET"), false)
	}
	l, ok := e.listFor(args[0])
	if !ok {
		return e.encode(errors.New("ERR no such key"), false)
	}
	index, valid := counterInteger(args[1])
	if !valid {
		return e.encode(errNotAnInteger, false)
	}
	if !l.Set(int(index), args[2]) {
		return e.encode(errors.New("ERR index out of range"), false)
	}
	e.listStore.Resize(args[0])
	return constant.RespOk
}

// cmdLRANGE answers the elements between two positions, both inclusive and both
// allowed to be negative or out of range - a range outside the list is empty
// rather than an error, which is what makes LRANGE key 0 -1 the idiom for
// "everything" whatever the length.
func (e *Engine) cmdLRANGE(args []string) []byte {
	if len(args) != 3 {
		return e.encode(wrongArguments("LRANGE"), false)
	}
	start, stop, err := integerRange(args[1], args[2])
	if err != nil {
		return e.encode(err, false)
	}

	l, ok := e.listFor(args[0])
	if !ok {
		return constant.RespEmptyArray
	}
	return e.encodeWalkReply(func(yield func(string) bool) { l.VisitRange(start, stop, yield) }, shapeArray)
}

func (e *Engine) cmdLTRIM(args []string) []byte {
	if len(args) != 3 {
		return e.encode(wrongArguments("LTRIM"), false)
	}
	start, stop, err := integerRange(args[1], args[2])
	if err != nil {
		return e.encode(err, false)
	}
	l, ok := e.listFor(args[0])
	if !ok {
		return constant.RespOk
	}
	values := l.Range(start, stop)
	if len(values) == 0 {
		e.listStore.Delete(args[0])
		return constant.RespOk
	}
	ttl, expires := e.listStore.GetExpiry(args[0])
	replacement := data_structure.NewList()
	replacement.PushBack(values...)
	e.listStore.Put(args[0], replacement)
	if expires {
		e.listStore.SetExpiryAt(args[0], ttl)
	}
	return constant.RespOk
}
