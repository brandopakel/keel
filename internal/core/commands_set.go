package core

import (
	"errors"
	"math"
	"strconv"

	"github.com/brandopakel/keel/internal/constant"
	"github.com/brandopakel/keel/internal/data_structure"
)

// Set commands.
//
// Redis has no empty set: removing the last member removes the key, so every
// command that takes members out goes through setSettle, which drops an emptied
// set and otherwise re-measures it for the memory budget.

func setFor(key string) (*data_structure.Set, bool) {
	return setStore.Get(key)
}

// setSettle records that a set was changed in place.
func setSettle(key string, s *data_structure.Set) {
	if s.Len() == 0 {
		setStore.Delete(key)
		return
	}
	setStore.Resize(key)
}

var (
	errIntegerOutOfRange = errors.New("ERR value is not an integer or out of range")
	errCountNegative     = errors.New("ERR value is out of range, must be positive")
)

// maxRandomMemberCount bounds what SRANDMEMBER may be asked for with a negative
// count. A positive count is capped by the size of the set, but a negative one
// asks for exactly that many with repeats, and the reply is built before it is
// written - so the count sizes an allocation, and a client could ask for one
// no server could make. Sixteen million members is past any use of the command.
const maxRandomMemberCount = 1 << 24

func cmdSADD(args []string) []byte {
	if len(args) < 2 {
		return Encode(errors.New("ERR wrong number of arguments for 'SADD' command"), false)
	}
	key := args[0]
	s, ok := setFor(key)
	if !ok {
		s = data_structure.NewSet()
		setStore.Put(key, s)
	}
	added := s.Add(args[1:]...)
	// Members change the set's size without going through Put, so the keyspace
	// has to re-measure it or the memory budget keeps believing the old figure.
	setStore.Resize(key)
	return Encode(added, false)
}

func cmdSREM(args []string) []byte {
	if len(args) < 2 {
		return Encode(errors.New("ERR wrong number of arguments for 'SREM' command"), false)
	}
	key := args[0]
	s, ok := setFor(key)
	if !ok {
		// Nothing to remove from, and nothing is created to say so.
		return constant.RespZero
	}
	removed := s.Remove(args[1:]...)
	setSettle(key, s)
	return Encode(removed, false)
}

func cmdSCARD(args []string) []byte {
	if len(args) != 1 {
		return Encode(errors.New("ERR wrong number of arguments for 'SCARD' command"), false)
	}
	s, ok := setFor(args[0])
	if !ok {
		return constant.RespZero
	}
	return Encode(s.Len(), false)
}

func cmdSMEMBERS(args []string) []byte {
	if len(args) != 1 {
		return Encode(errors.New("ERR wrong number of arguments for 'SMEMBERS' command"), false)
	}
	s, ok := setFor(args[0])
	if !ok {
		return constant.RespEmptyArray
	}
	return Encode(s.Members(), false)
}

func cmdSISMEMBER(args []string) []byte {
	if len(args) != 2 {
		return Encode(errors.New("ERR wrong number of arguments for 'SISMEMBER' command"), false)
	}
	s, ok := setFor(args[0])
	if !ok || !s.Contains(args[1]) {
		return constant.RespZero
	}
	return constant.RespOne
}

// cmdSMISMEMBER answers one integer per member asked about, in the order asked.
func cmdSMISMEMBER(args []string) []byte {
	if len(args) < 2 {
		return Encode(errors.New("ERR wrong number of arguments for 'SMISMEMBER' command"), false)
	}
	s, ok := setFor(args[0])
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

// parseCount reads the optional count SPOP and SRANDMEMBER take, reporting
// whether one was given at all: without one they answer a single member rather
// than an array of one.
func parseCount(args []string) (count int64, given bool, err error) {
	if len(args) < 2 {
		return 0, false, nil
	}
	n, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil || n == math.MinInt64 {
		return 0, true, errIntegerOutOfRange
	}
	return n, true, nil
}

// cmdSPOP implements SPOP key [count]: it removes and returns random members.
func cmdSPOP(args []string) []byte {
	if len(args) != 1 && len(args) != 2 {
		return Encode(errors.New("ERR wrong number of arguments for 'SPOP' command"), false)
	}
	key := args[0]
	count, given, err := parseCount(args)
	if err != nil {
		return Encode(err, false)
	}
	if count < 0 {
		return Encode(errCountNegative, false)
	}

	s, ok := setFor(key)
	if !ok {
		if given {
			return constant.RespEmptyArray
		}
		return constant.RespNil
	}

	// Without a count the command pops one member, not zero. Asking for zero
	// and then reading the first of the nothing that came back is what made
	// plain SPOP panic, and a panic here is the whole server rather than the
	// connection that sent it.
	want := count
	if !given {
		want = 1
	}
	popped := s.Pop(int(want))

	// Replaying SPOP would pop different members, so the log records the
	// removal that actually happened rather than the request that caused it.
	if len(popped) > 0 {
		aofRecord(append([]string{"SREM", key}, popped...)...)
	}
	setSettle(key, s)

	if !given {
		if len(popped) == 0 {
			return constant.RespNil
		}
		return Encode(popped[0], false)
	}
	return Encode(popped, false)
}

// cmdSRANDMEMBER implements SRANDMEMBER key [count], and answers SRAND, the
// name this server used for it before. A positive count returns up to that
// many distinct members; a negative one returns exactly that many, drawn
// independently, so the same member may come back more than once.
func cmdSRANDMEMBER(args []string) []byte {
	if len(args) != 1 && len(args) != 2 {
		return Encode(errors.New("ERR wrong number of arguments for 'SRANDMEMBER' command"), false)
	}
	count, given, err := parseCount(args)
	if err != nil {
		return Encode(err, false)
	}

	s, ok := setFor(args[0])
	if !ok {
		if given {
			return constant.RespEmptyArray
		}
		return constant.RespNil
	}
	if !given {
		// One member, for the same reason SPOP takes one.
		picked := s.RandomMembers(1)
		if len(picked) == 0 {
			return constant.RespNil
		}
		return Encode(picked[0], false)
	}
	if count < 0 {
		if -count > maxRandomMemberCount {
			return Encode(errIntegerOutOfRange, false)
		}
		return Encode(s.RandomMembersWithRepeats(int(-count)), false)
	}
	if count > maxRandomMemberCount {
		count = maxRandomMemberCount
	}
	return Encode(s.RandomMembers(int(count)), false)
}
