package core

import (
	"github.com/brandopakel/keel/internal/constant"
	"github.com/brandopakel/keel/internal/resp"
)

// The list commands' command language: each reads its arguments as Redis
// reads them, calls its typed operation (lists.go), and answers through the
// engine's reply sink. A write records itself; it is never logged as it
// arrived.

// push is LPUSH and RPUSH, which differ only in the end they add to.
func (e *Engine) push(args []string, front bool, name string) []byte {
	e.recordsItself()
	if len(args) < 2 {
		return e.encode(wrongArguments(name), false)
	}
	r := e.reply()
	return r.Int(e.opPush(args[0], args[1:], front)).Bytes()
}

func (e *Engine) cmdLPUSH(args []string) []byte { return e.push(args, true, "LPUSH") }
func (e *Engine) cmdRPUSH(args []string) []byte { return e.push(args, false, "RPUSH") }

// pop is LPOP and RPOP. Without a count it answers one element; with one it
// answers an array, which is Redis 6.2's behaviour and the one clients test
// for - a count of zero is an empty array rather than a nil.
//
// The count is read before the key is looked at, as Redis reads it. The
// elements are answered, and their reply admitted, before they are removed:
// a reply that cannot be sent leaves the list as it was.
func (e *Engine) pop(args []string, front bool, name string) []byte {
	e.recordsItself()
	if len(args) < 1 || len(args) > 2 {
		return e.encode(wrongArguments(name), false)
	}
	req := listPop{front: front, counted: len(args) == 2}
	if req.counted {
		n, err := e.positiveCount(args[1])
		if err != nil {
			return e.encode(err, false)
		}
		req.count = n
	}
	key := args[0]

	l, ok := e.listFor(key)
	if !ok {
		e.opPop(key, nil, req)
		// A null array and a null bulk string are different replies, and which
		// one belongs here depends on what the command was going to answer.
		// Checked against Redis 8.10.1, which sends *-1 for the counted form
		// and $-1 for the bare one; sending $-1 for both is a type error in any
		// client that decodes the counted reply into a list. RESP3 has one
		// null for both.
		if req.counted {
			return e.wire().NullArray()
		}
		return e.wire().Null()
	}

	if !req.counted {
		// One element is a reply of one bulk string, answered as GET's is
		// (resp.SingleBulk).
		end := 0
		if !front {
			end = -1
		}
		value, _ := l.Index(end)
		out := resp.SingleBulk(MaxReplyBytes-e.replyCeiling, e.commandAllocations, value)
		if out[0] == '-' {
			return out
		}
		e.opPop(key, l, req)
		return out
	}

	count := req.popped(l.Len())
	out := e.encodeWalkReply(func(yield func(string) bool) {
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
	}, shapeArray)
	if len(out) > 0 && out[0] == '-' {
		return out
	}
	e.opPop(key, l, req)
	return out
}

func (e *Engine) cmdLPOP(args []string) []byte { return e.pop(args, true, "LPOP") }
func (e *Engine) cmdRPOP(args []string) []byte { return e.pop(args, false, "RPOP") }

func (e *Engine) cmdLLEN(args []string) []byte {
	if len(args) != 1 {
		return e.encode(wrongArguments("LLEN"), false)
	}
	r := e.reply()
	return r.Int(e.opLLen(args[0])).Bytes()
}

// cmdLINDEX answers a key that is not there with nil whatever the index says,
// because Redis looks the key up before it reads the index.
func (e *Engine) cmdLINDEX(args []string) []byte {
	if len(args) != 2 {
		return e.encode(wrongArguments("LINDEX"), false)
	}
	index, valid := e.counterInteger(args[1])
	if !valid {
		if _, ok := e.listFor(args[0]); !ok {
			return e.wire().Null()
		}
		return e.encode(errNotAnInteger, false)
	}
	// One element, answered as GET's is (resp.SingleBulk).
	if value, ok := e.opLIndex(args[0], index); ok {
		return resp.SingleBulk(MaxReplyBytes-e.replyCeiling, e.commandAllocations, value)
	}
	return e.wire().Null()
}

// cmdLSET, like LINDEX, refuses a key that is not there before it reads the
// index.
func (e *Engine) cmdLSET(args []string) []byte {
	e.recordsItself()
	if len(args) != 3 {
		return e.encode(wrongArguments("LSET"), false)
	}
	r := e.reply()
	index, valid := e.counterInteger(args[1])
	if !valid {
		if _, ok := e.listFor(args[0]); !ok {
			return r.Error(errNoSuchKey).Bytes()
		}
		return r.Error(errNotAnInteger).Bytes()
	}
	if err := e.opLSet(args[0], index, args[2]); err != nil {
		return r.Error(err).Bytes()
	}
	return r.Status("OK").Bytes()
}

// cmdLRANGE answers the elements between two positions, both inclusive and both
// allowed to be negative or out of range - a range outside the list is empty
// rather than an error, which is what makes LRANGE key 0 -1 the idiom for
// "everything" whatever the length. The positions are read before the key is
// looked at, as Redis reads them.
func (e *Engine) cmdLRANGE(args []string) []byte {
	if len(args) != 3 {
		return e.encode(wrongArguments("LRANGE"), false)
	}
	start, stop, err := e.integerRange(args[1], args[2])
	if err != nil {
		return e.encode(err, false)
	}
	rng, ok := e.opLRange(args[0], start, stop)
	if !ok {
		return constant.RespEmptyArray
	}
	return e.encodeWalkReply(rng.visit, shapeArray)
}

// cmdLTRIM reads its positions before the key, as LRANGE does; see opLTrim.
func (e *Engine) cmdLTRIM(args []string) []byte {
	e.recordsItself()
	if len(args) != 3 {
		return e.encode(wrongArguments("LTRIM"), false)
	}
	start, stop, err := e.integerRange(args[1], args[2])
	if err != nil {
		return e.encode(err, false)
	}
	e.opLTrim(args[0], start, stop)
	r := e.reply()
	return r.Status("OK").Bytes()
}
