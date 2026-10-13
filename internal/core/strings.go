package core

import (
	"errors"
	"math"
	"strconv"

	"github.com/brandopakel/keel/internal/data_structure"
	"github.com/brandopakel/keel/internal/resp"
)

// Typed operations: strings (phase 4 of docs/embedding-plan.md, step 4.3).
//
// A typed operation is what a command does, with Go types in, a typed result
// and a typed error out, and the records of what it changed staged for the
// log. It parses nothing and answers nobody: the command language
// (commands_dict.go and its kin) reads a command's arguments, calls the
// operation, and writes its result into a resp.Reply; phase 5's public
// methods call it with Go values. So the log holds what the operation
// staged, whoever called it, and a command converted to typed operations is
// never logged as it arrived.
//
// An operation runs in a command's scope: the engine's lock held, the log's
// staging begun (aofBegin) and committed after it (aofCommit), eviction held
// off around it - the scope EvalAndResponse gives a command. And it runs
// after the checks that scope makes from the command's descriptor: its
// count, a replica's refusal, and the type of the keys it names. A typed
// operation therefore does not ask what type holds a key the descriptor
// constrains: GET's caller has refused a hash. What it refuses on its own is
// what only it can know: SET's GET finding a key of another type, a value
// that is not an integer, an increment that would overflow.
//
// The handler of a converted command declares that it records itself
// (recordsItself), so the command is never logged as it arrived: what its
// operation staged is what the log holds, and nothing when it changed
// nothing.
//
// Its errors are the sentinels whose text is Redis's wire error, so the
// command language answers them as they are, and phase 5 matches them with
// errors.Is.

var (
	// errLCSTooLong refuses an LCS whose table would pass the engine's
	// LCSMaxCells, in Redis's words.
	errLCSTooLong = errors.New("ERR String too long for LCS")
	// errCommandBudget is the transport's allocation budget refusing a
	// command's workspace, as allocationPressure answers it.
	errCommandBudget = errors.New("ERR temporary command allocation budget exhausted")
)

// reply is the sink the running command answers into: framed in the
// command's protocol, and admitted as admitReply admits a reply, within the
// engine's reply ceiling and reserved from the transport's budget.
func (e *Engine) reply() resp.Reply {
	return resp.Reply{Framing: e.wire(), Used: MaxReplyBytes - e.replyCeiling, Budget: e.commandAllocations}
}

// opGet is the string key holds, and false when it holds none: GET, and each
// key of MGET. A key whose expiry has passed is reaped, and holds none.
func (e *Engine) opGet(key string) (string, bool) {
	obj := e.dictStore.Get(key)
	if obj == nil {
		return "", false
	}
	return obj.Value, true
}

// stringTarget is what SET finds at a key before it writes: the string held
// there, or the keyspace of another type that holds the name, or neither.
type stringTarget struct {
	obj   *data_structure.Obj
	other data_structure.Keyspace
}

// exists reports whether anything holds the name, a string or not.
func (t stringTarget) exists() bool { return t.obj != nil || t.other != nil }

// setTarget looks key up for SET: the string store first, and the others only
// if it holds no string. A name another type holds is a key that exists, so
// NX leaves it and XX replaces it, as in Redis (see replacingWrites).
func (e *Engine) setTarget(key string) stringTarget {
	if obj := e.dictStore.Get(key); obj != nil {
		return stringTarget{obj: obj}
	}
	if other, held := e.space.OwnerOf(key); held && other.KeyspaceName() != e.dictStore.KeyspaceName() {
		return stringTarget{other: other}
	}
	return stringTarget{}
}

// setRequest is what a SET asks for beyond storing its value.
type setRequest struct {
	// nx and xx store the value only if the key does not exist, or does.
	nx, xx bool
	// keepTTL keeps the expiry the name has, a key of another type's too.
	keepTTL bool
	// expires sets the expiry at, in Unix milliseconds.
	expires bool
	at      int64
}

// opSet stores value at key, as SET does, given what setTarget found there,
// and reports whether it stored it: NX and XX can decline, which records
// nothing. A key of another type is deleted first, and its DEL recorded
// before the SET (dropOtherType); an expiry is recorded as the instant it
// falls due.
func (e *Engine) opSet(t stringTarget, key, value string, req setRequest) bool {
	exists := t.exists()
	if (req.nx && exists) || (req.xx && !exists) {
		return false
	}
	at, expires := req.at, req.expires
	if req.keepTTL && exists {
		// KEEPTTL keeps the key's expiry, and the key is the name: a hash
		// replaced by a string under KEEPTTL keeps the hash's deadline.
		var old uint64
		var has bool
		if t.obj != nil {
			old, has = e.dictStore.GetExpiry(key)
		} else {
			old, has = t.other.GetExpiry(key)
		}
		if has {
			at, expires = int64(old), true
		}
	}
	if t.other != nil {
		e.dropOtherType(t.other, key)
	}
	e.dictStore.Put(key, e.dictStore.NewObj(value))
	e.aofRecord("SET", key, value)
	if expires {
		e.dictStore.SetExpiryAt(key, uint64(at))
		e.aofRecordInt("PEXPIREAT", key, at)
	}
	return true
}

// opMSet stores each value of pairs, key value key value, as MSET does: a
// key another type holds is replaced, its DEL recorded first, and the MSET
// is recorded whole after them.
func (e *Engine) opMSet(pairs []string) {
	for i := 0; i < len(pairs); i += 2 {
		key, value := pairs[i], pairs[i+1]
		if other, held := e.space.OwnerOf(key); held && other.KeyspaceName() != e.dictStore.KeyspaceName() {
			e.dropOtherType(other, key)
		}
		e.dictStore.Put(key, e.dictStore.NewObj(value))
	}
	e.aofRecordArgs("MSET", pairs)
}

// opIncr, opDecr, opIncrBy and opDecrBy add to the integer a key holds,
// treating a missing key as zero, and answer the value after it, each
// recording itself. The value is changed in place, so a TTL on the key
// survives, as it does in Redis. A value that is not a canonical integer is
// refused, and so is one that would overflow, rather than wrapping to a
// number nobody asked for.
func (e *Engine) opIncr(key string) (int64, error) {
	n, err := e.addToInteger(key, 1)
	if err == nil {
		e.aofRecord("INCR", key)
	}
	return n, err
}

func (e *Engine) opDecr(key string) (int64, error) {
	n, err := e.addToInteger(key, -1)
	if err == nil {
		e.aofRecord("DECR", key)
	}
	return n, err
}

func (e *Engine) opIncrBy(key string, by int64) (int64, error) {
	n, err := e.addToInteger(key, by)
	if err == nil {
		e.aofRecordInt("INCRBY", key, by)
	}
	return n, err
}

// opDecrBy refuses the one decrement that cannot be negated before it looks
// at the key, as Redis does.
func (e *Engine) opDecrBy(key string, by int64) (int64, error) {
	if by == math.MinInt64 {
		return 0, errDecrOverflow
	}
	n, err := e.addToInteger(key, -by)
	if err == nil {
		e.aofRecordInt("DECRBY", key, by)
	}
	return n, err
}

// addToInteger adds delta to the integer key holds, and answers the sum.
func (e *Engine) addToInteger(key string, delta int64) (int64, error) {
	obj := e.dictStore.Get(key)
	current := int64(0)
	if obj != nil {
		var valid bool
		current, valid = canonicalInteger(obj.Value)
		if !valid {
			return 0, errNotAnInteger
		}
	}
	if (delta > 0 && current > math.MaxInt64-delta) || (delta < 0 && current < math.MinInt64-delta) {
		return 0, errIncrOverflow
	}
	current += delta
	value := strconv.FormatInt(current, 10)
	if obj == nil {
		e.dictStore.Put(key, e.dictStore.NewObj(value))
	} else {
		e.dictStore.UpdateValue(key, value)
	}
	return current, nil
}

// lcsRequest is what an LCS asks for: the length only, or the matching
// ranges (IDX), and of those only the ranges at least minMatchLen long.
type lcsRequest struct {
	lengthOnly, ranges bool
}

// lcsResult is what LCS found: the subsequence, its length, and with IDX
// the matching ranges, last match first, the order Redis reports them in.
type lcsResult struct {
	sequence string
	length   int64
	matches  []data_structure.LCSMatch
}

// opLCS finds the longest common subsequence of the strings keys a and b
// hold, a missing key holding the empty string, as Redis compares one. The
// computation costs the product of the two lengths, so it is bounded by the
// engine's LCSMaxCells, and its workspace is reserved from the transport's
// budget before it is built.
func (e *Engine) opLCS(a, b string, req lcsRequest) (lcsResult, error) {
	x, y := e.lcsValue(a), e.lcsValue(b)
	if e.space.LCSTooLarge(x, y) {
		// Redis's message, so a client that already handles this from Redis
		// handles it here. The reason underneath differs - Redis runs out of
		// room for its table, this runs out of time budget - but the client's
		// options are the same either way.
		return lcsResult{}, errLCSTooLong
	}
	if req.lengthOnly {
		if !e.reserveCommandMemory(8*(min(len(x), len(y))+1) + 8192) {
			return lcsResult{}, errCommandBudget
		}
		// Only the length is wanted, so nothing has to be recovered and the
		// two-row form does half the work.
		return lcsResult{length: int64(data_structure.LCSLen(x, y))}, nil
	}
	// The checkpointed rows, pair/run slice growth, the subsequence and the
	// encoded output coexist. IDX also builds nested arrays.
	perByte := 128
	if req.ranges {
		perByte = 1024
	}
	shorter := min(len(x), len(y))
	maxInt := int(^uint(0) >> 1)
	base := data_structure.LCSWorkspaceBytes(len(x), len(y)) + 16384
	if shorter == 0 {
		base = 64
	}
	if shorter > (maxInt-base)/perByte || !e.reserveCommandMemory(base+perByte*shorter) {
		return lcsResult{}, errCommandBudget
	}
	matches, seq := data_structure.LCSMatches(x, y)
	return lcsResult{sequence: seq, length: int64(len(seq)), matches: matches}, nil
}

// lcsValue reads a key as a string, a missing key as the empty one: Redis
// compares against an empty string for a key that is not there, which makes
// LCS of a key and a missing one answer nothing rather than an error. A key
// holding another type never reaches here: the type check refuses it first,
// in the words Redis's LCS uses (see typeError).
func (e *Engine) lcsValue(key string) string {
	obj := e.dictStore.Get(key)
	if obj == nil || e.dictStore.HasExpired(key) {
		return ""
	}
	return obj.Value
}
