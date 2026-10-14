package core

import (
	"errors"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"

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
	// expires sets the expiry at, in Unix milliseconds, which absolute says
	// was given as an instant (EXAT, PXAT), and so may have passed.
	expires, absolute bool
	at                int64
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
	if req.expires && req.absolute && req.at <= e.commandClock() && !e.aof.replaying && !e.replicaApplying {
		// An instant that has passed stores nothing, as in Redis's
		// setGenericCommand: the key there is deleted, recorded as DEL, and
		// the key is counted as expired, as if stored and expired at once
		// (t_string.c). A log's replay and a replica store what they are
		// given, as Redis's checkAlreadyExpired does while loading or on a
		// replica.
		switch {
		case t.other != nil:
			e.dropOtherType(t.other, key)
		case t.obj != nil:
			e.dictStore.Delete(key)
			e.aofRecord("DEL", key)
		}
		e.space.NoteExpired()
		return true
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

// The string commands Keel did not have (docs/redis-parity-plan.md), as
// Redis 8.10.1's t_string.c defines each. Each records itself in commands
// the builds before it know where that costs nothing - GETSET as SET,
// GETDEL as DEL, GETEX as PEXPIREAT, DEL or PERSIST, INCRBYFLOAT as SET with
// KEEPTTL, MSETNX as MSET - and APPEND and SETRANGE as themselves, as Redis
// does, because writing them as SET would copy the whole value into the log
// on every call.

var (
	// errStringTooLong is Redis's refusal of a string longer than its
	// proto-max-bulk-len, 512 MiB, which is also the longest bulk string a
	// client may send Keel.
	errStringTooLong = errors.New("ERR string exceeds maximum allowed size (proto-max-bulk-len)")
	errOffsetRange   = errors.New("ERR offset is out of range")
	errFloatInfinite = errors.New("ERR increment would produce NaN or Infinity")
)

// stringFits is Redis's checkStringLength: whether a string of size grown by
// more stays within the longest a command may make. A log's replay and a
// replica apply what their source accepted, as Redis's mustObeyClient does.
func (e *Engine) stringFits(size, more int64) bool {
	if e.aof.replaying || e.replicaApplying {
		return true
	}
	total := uint64(size) + uint64(more)
	return total <= maxBulkLength
}

// opAppend appends value to the string key holds, creating the key, and
// answers its new length. The key keeps its expiry.
func (e *Engine) opAppend(key, value string) (int64, error) {
	obj := e.dictStore.Get(key)
	if obj == nil {
		e.dictStore.Put(key, e.dictStore.NewObj(value))
		e.aofRecord("APPEND", key, value)
		return int64(len(value)), nil
	}
	if !e.stringFits(int64(len(obj.Value)), int64(len(value))) {
		return 0, errStringTooLong
	}
	joined := obj.Value + value
	e.dictStore.UpdateValue(key, joined)
	e.aofRecord("APPEND", key, value)
	return int64(len(joined)), nil
}

// opStrlen is the length of the string key holds, 0 for none.
func (e *Engine) opStrlen(key string) int64 {
	obj := e.dictStore.Get(key)
	if obj == nil {
		return 0
	}
	return int64(len(obj.Value))
}

// opGetRange is the part of the string key holds from start to end, both
// inclusive, either counted from the end when negative, as Redis's
// getrangeCommand reads them: empty for a missing key, or a range that
// selects nothing.
func (e *Engine) opGetRange(key string, start, end int64) string {
	obj := e.dictStore.Get(key)
	// A keyspace hit or a miss, counted here, after the indexes were read,
	// as Redis's getrangeCommand reads them before it looks the key up.
	e.noteLookup(obj != nil)
	if obj == nil {
		return ""
	}
	s := obj.Value
	n := int64(len(s))
	if start < 0 && end < 0 && start > end {
		return ""
	}
	if start < 0 {
		start = n + start
	}
	if end < 0 {
		end = n + end
	}
	start, end = max(start, 0), max(end, 0)
	if end >= n {
		end = n - 1
	}
	if start > end || n == 0 {
		return ""
	}
	return s[start : end+1]
}

// opSetRange writes value into the string key holds at offset, padding with
// zero bytes, and answers the string's length. An empty value changes
// nothing, creates no key, and records nothing.
func (e *Engine) opSetRange(key string, offset int64, value string) (int64, error) {
	obj := e.dictStore.Get(key)
	var old string
	if obj != nil {
		old = obj.Value
	}
	if len(value) == 0 {
		return int64(len(old)), nil
	}
	if !e.stringFits(offset, int64(len(value))) {
		return 0, errStringTooLong
	}
	end := offset + int64(len(value))
	length := max(int64(len(old)), end)
	var b strings.Builder
	b.Grow(int(length))
	if offset <= int64(len(old)) {
		b.WriteString(old[:offset])
	} else {
		b.WriteString(old)
		writeZeros(&b, offset-int64(len(old)))
	}
	b.WriteString(value)
	if end < int64(len(old)) {
		b.WriteString(old[end:])
	}
	if obj == nil {
		e.dictStore.Put(key, e.dictStore.NewObj(b.String()))
	} else {
		e.dictStore.UpdateValue(key, b.String())
	}
	e.aofRecord("SETRANGE", key, strconv.FormatInt(offset, 10), value)
	return length, nil
}

// writeZeros writes n zero bytes to b, a block at a time.
func writeZeros(b *strings.Builder, n int64) {
	var zeros [4096]byte
	for n > 0 {
		chunk := min(n, int64(len(zeros)))
		b.Write(zeros[:chunk])
		n -= chunk
	}
}

// opGetDel answers the string key holds and deletes the key: false, and
// nothing recorded, for none.
func (e *Engine) opGetDel(key string) (string, bool) {
	value, ok := e.opGet(key)
	if ok {
		e.deleteString(key)
	}
	return value, ok
}

// deleteString deletes the string key holds, recorded as the DEL it is, as
// Redis propagates GETDEL.
func (e *Engine) deleteString(key string) {
	e.dictStore.Delete(key)
	e.aofRecord("DEL", key)
}

// getExRequest is what a GETEX does to the key's expiry: set it at an
// instant, which an absolute one (EXAT, PXAT) may have passed, or remove it
// (PERSIST), or nothing.
type getExRequest struct {
	expires, absolute, persist bool
	at                         int64
}

// opGetExExpiry applies a GETEX's expiry to a key that holds a string, which
// the command has already answered. An instant that has passed deletes the
// key, recorded as DEL; an instant to come is recorded as PEXPIREAT; PERSIST
// is recorded only when there was an expiry to remove. A log's replay and a
// replica do not delete for an instant that has passed, as Redis's
// checkAlreadyExpired does not while loading or on a replica.
func (e *Engine) opGetExExpiry(key string, req getExRequest) {
	switch {
	case req.expires && req.absolute && req.at <= time.Now().UnixMilli() && !e.aof.replaying && !e.replicaApplying:
		e.dictStore.Delete(key)
		e.aofRecord("DEL", key)
	case req.expires:
		e.dictStore.SetExpiryAt(key, uint64(req.at))
		e.aofRecordInt("PEXPIREAT", key, req.at)
	case req.persist && e.dictStore.ClearExpiry(key):
		e.aofRecord("PERSIST", key)
	}
}

// opGetSet stores value at key, as SET does without options, and answers
// the string it held: false for none.
func (e *Engine) opGetSet(key, value string) (string, bool) {
	old, ok := e.opGet(key)
	e.storeString(key, value)
	return old, ok
}

// storeString stores value at a key that holds a string or nothing, its
// expiry dropped, recorded as the SET it is, as Redis propagates GETSET.
func (e *Engine) storeString(key, value string) {
	e.dictStore.Put(key, e.dictStore.NewObj(value))
	e.aofRecord("SET", key, value)
}

// opMSetNX stores each value of pairs only if none of the keys exists, of
// any type, as Redis's MSETNX does, and reports whether it stored them. It
// is recorded as the MSET it then is.
func (e *Engine) opMSetNX(pairs []string) bool {
	for i := 0; i < len(pairs); i += 2 {
		if _, held := e.space.OwnerOf(pairs[i]); held {
			return false
		}
	}
	for i := 0; i < len(pairs); i += 2 {
		e.dictStore.Put(pairs[i], e.dictStore.NewObj(pairs[i+1]))
	}
	e.aofRecordArgs("MSET", pairs)
	return true
}

// opIncrByFloat adds by to the number the string key holds, a missing key
// being 0, in Redis's long double (long_double.go), and answers the sum as
// it stores it.
func (e *Engine) opIncrByFloat(key string, by *big.Float) (string, error) {
	value, exists, err := e.floatSum(key, by)
	if err == nil {
		e.storeFloatSum(key, value, exists)
	}
	return value, err
}

// floatSum is the text INCRBYFLOAT would store at key, and whether the key
// exists, with nothing written yet, so that the command can admit its reply
// first.
func (e *Engine) floatSum(key string, by *big.Float) (value string, exists bool, err error) {
	obj := e.dictStore.Get(key)
	current := new(big.Float).SetPrec(longDoublePrec)
	if obj != nil {
		var ok bool
		if current, ok = parseLongDouble(obj.Value); !ok {
			return "", true, errNotAFloat
		}
	}
	sum, finite := addLongDouble(current, by)
	if !finite {
		return "", obj != nil, errFloatInfinite
	}
	return formatLongDoubleHuman(sum), obj != nil, nil
}

// storeFloatSum stores what floatSum made, the key keeping its expiry. It is
// recorded as SET with KEEPTTL, as Redis propagates INCRBYFLOAT, so that a
// replay stores the same text whatever its arithmetic.
func (e *Engine) storeFloatSum(key, value string, exists bool) {
	if exists {
		e.dictStore.UpdateValue(key, value)
	} else {
		e.dictStore.Put(key, e.dictStore.NewObj(value))
	}
	e.aofRecord("SET", key, value, "KEEPTTL")
}
