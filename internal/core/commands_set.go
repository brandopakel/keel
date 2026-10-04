package core

import (
	"errors"
	"math"

	"github.com/brandopakel/keel/internal/constant"
	"github.com/brandopakel/keel/internal/data_structure"
)

// Set commands.
//
// Redis has no empty set: removing the last member removes the key, so every
// command that takes members out goes through setSettle, which drops an emptied
// set and otherwise re-measures it for the memory budget.

func (e *Engine) setFor(key string) (*data_structure.Set, bool) {
	return e.setStore.Get(key)
}

// setSettle records that a set was changed in place.
func (e *Engine) setSettle(key string, s *data_structure.Set) {
	if s.Len() == 0 {
		e.setStore.Delete(key)
		return
	}
	e.setStore.Resize(key)
}

var (
	errIntegerOutOfRange = errors.New("ERR value is not an integer or out of range")
	// Redis's words for a count past what it can negate, as they are.
	errRandomCountRange = errors.New("ERR value is out of range, value must between -9223372036854775807 and 9223372036854775807")
)

// maxRandomMemberCount bounds what SRANDMEMBER may be asked for with a negative
// count. A positive count is capped by the size of the set, but a negative one
// asks for exactly that many with repeats, and the reply is built before it is
// written - so the count sizes an allocation, and a client could ask for one
// no server could make. Sixteen million members is past any use of the command.
const maxRandomMemberCount = 1 << 24

func (e *Engine) cmdSADD(args []string) []byte {
	if len(args) < 2 {
		return Encode(wrongArguments("SADD"), false)
	}
	key := args[0]
	s, ok := e.setFor(key)
	if !ok {
		s = data_structure.NewSet()
		e.setStore.Put(key, s)
	}
	added := s.Add(args[1:]...)
	// Members change the set's size without going through Put, so the keyspace
	// has to re-measure it or the memory budget keeps believing the old figure.
	e.setStore.Resize(key)
	return Encode(added, false)
}

func (e *Engine) cmdSREM(args []string) []byte {
	if len(args) < 2 {
		return Encode(wrongArguments("SREM"), false)
	}
	key := args[0]
	s, ok := e.setFor(key)
	if !ok {
		// Nothing to remove from, and nothing is created to say so.
		return constant.RespZero
	}
	removed := s.Remove(args[1:]...)
	e.setSettle(key, s)
	return Encode(removed, false)
}

func (e *Engine) cmdSCARD(args []string) []byte {
	if len(args) != 1 {
		return Encode(wrongArguments("SCARD"), false)
	}
	s, ok := e.setFor(args[0])
	if !ok {
		return constant.RespZero
	}
	return Encode(s.Len(), false)
}

// cmdSMEMBERS answers every member, as a set.
func (e *Engine) cmdSMEMBERS(args []string) []byte {
	if len(args) != 1 {
		return Encode(wrongArguments("SMEMBERS"), false)
	}
	s, ok := e.setFor(args[0])
	if !ok {
		return emptySetReply()
	}
	return e.encodeLookupArray(s.Len(), s.MemberAt, shapeSet)
}

func (e *Engine) cmdSISMEMBER(args []string) []byte {
	if len(args) != 2 {
		return Encode(wrongArguments("SISMEMBER"), false)
	}
	s, ok := e.setFor(args[0])
	if !ok || !s.Contains(args[1]) {
		return constant.RespZero
	}
	return constant.RespOne
}

// cmdSMISMEMBER answers one integer per member asked about, in the order asked.
func (e *Engine) cmdSMISMEMBER(args []string) []byte {
	if len(args) < 2 {
		return Encode(wrongArguments("SMISMEMBER"), false)
	}
	s, ok := e.setFor(args[0])
	out := make([]interface{}, 0, len(args)-1)
	for _, member := range args[1:] {
		if ok && s.Contains(member) {
			out = append(out, int64(1))
		} else {
			out = append(out, int64(0))
		}
	}
	return Encode(out, false)
}

// randomCount reads SRANDMEMBER's optional count, reporting whether one was
// given at all: without one it answers a single member rather than an array
// of one. Any count is allowed whose negation is one too.
func randomCount(args []string) (count int64, given bool, err error) {
	if len(args) < 2 {
		return 0, false, nil
	}
	n, valid := counterInteger(args[1])
	if !valid {
		return 0, true, errIntegerOutOfRange
	}
	if n == math.MinInt64 {
		return 0, true, errRandomCountRange
	}
	return n, true, nil
}

// cmdSPOP implements SPOP key [count]: it removes and returns random members,
// as a set when a count was given - which is Redis's reply, though
// SRANDMEMBER's with a count is an array. The count is read before the key is
// looked up, and more than one is Redis's syntax error.
func (e *Engine) cmdSPOP(args []string) []byte {
	if len(args) < 1 {
		return Encode(wrongArguments("SPOP"), false)
	}
	if len(args) > 2 {
		return Encode(errSyntax, false)
	}
	key := args[0]
	var count int64
	given := len(args) == 2
	if given {
		var err error
		if count, err = positiveCount(args[1]); err != nil {
			return Encode(err, false)
		}
	}

	s, ok := e.setFor(key)
	if !ok {
		if given {
			return emptySetReply()
		}
		return nullReply()
	}

	// Without a count the command pops one member, not zero. Asking for zero
	// and then reading the first of the nothing that came back is what made
	// plain SPOP panic, and a panic here is the whole server rather than the
	// connection that sent it.
	want := count
	if !given {
		want = 1
	}
	count = min(want, int64(s.Len()))
	// Shuffling positions invalidates a rewrite cursor even if admission fails.
	noteRewriteDirty(key)
	count = int64(s.ShufflePrefix(int(count)))
	walk := replyWalk(func(yield func(string) bool) {
		for i := 0; i < int(count); i++ {
			value, _ := s.MemberAt(i)
			if !yield(value) {
				return
			}
		}
	})
	if refusal := e.reserveRemoval("SREM", key, int(count), walk); refusal != nil {
		return refusal
	}
	shape := shapeSet
	if !given {
		shape = shapeOne
	}
	out := e.encodeWalkReply(walk, shape)
	if len(out) > 0 && out[0] == '-' {
		return out
	}
	if count > 0 {
		record := make([]string, 2, int(count)+2)
		record[0], record[1] = "SREM", key
		walk(func(value string) bool { record = append(record, value); return true })
		s.Remove(record[2:]...)
		aofRecord(record...)
	}
	e.setSettle(key, s)
	return out
}

// cmdSRANDMEMBER implements SRANDMEMBER key [count], and answers SRAND, the
// name this server used for it before. A positive count returns up to that
// many distinct members; a negative one returns exactly that many, drawn
// independently, so the same member may come back more than once.
func (e *Engine) cmdSRANDMEMBER(args []string) []byte {
	if len(args) < 1 {
		return Encode(wrongArguments("SRANDMEMBER"), false)
	}
	if len(args) > 2 {
		return Encode(errSyntax, false)
	}
	count, given, err := randomCount(args)
	if err != nil {
		return Encode(err, false)
	}

	s, ok := e.setFor(args[0])
	if !ok {
		if given {
			return constant.RespEmptyArray
		}
		return nullReply()
	}
	if !given || count > 0 {
		// Distinct sampling shuffles the internal order even though the set's
		// logical contents do not change. An incremental rewrite must restart.
		noteRewriteDirty(args[0])
	}
	if !given {
		// One member, for the same reason SPOP takes one.
		picked := s.RandomMembers(1)
		if len(picked) == 0 {
			return nullReply()
		}
		return Encode(picked[0], false)
	}
	if count < 0 {
		if -count > maxRandomMemberCount {
			return Encode(errIntegerOutOfRange, false)
		}
		return e.encodeRepeatedMembers(s, int(-count))
	}
	if count > maxRandomMemberCount {
		count = maxRandomMemberCount
	}
	count = int64(s.ShufflePrefix(int(count)))
	return e.encodeLookupArray(int(count), s.MemberAt, shapeArray)
}
