package core

import (
	"errors"
	"math"
	"math/big"
	"strconv"
	"time"

	"github.com/brandopakel/keel/internal/xxh3"
)

// The string operations Redis 8 added: a value's digest (DIGEST, 8.4), the
// conditions SET and DELEX compare a value or its digest against (8.4),
// MSETEX (8.4) and INCREX (8.8), as Redis 8.10.1's t_string.c and db.c
// define them.

var (
	// errDigestLength is Redis's validateHexDigest refusing a digest that
	// is not 16 characters long. Only the length is checked: anything else
	// simply does not match.
	errDigestLength = errors.New("ERR must be exactly 16 hexadecimal characters")
	// errDelexType refuses a condition on a key that does not hold a string.
	errDelexType      = errors.New("ERR Key should be of string type if conditions are specified")
	errDelexCondition = errors.New("ERR Invalid condition. Use IFEQ, IFNE, IFDEQ, or IFDNE")

	// errMsetexNumkeys is MSETEX's count refused; LMPOP's is errNumkeys.
	errMsetexNumkeys = errors.New("ERR invalid numkeys value")
	errKeyValuePairs = errors.New("ERR wrong number of key-value pairs")

	errIncrexIncrInteger   = errors.New("ERR Increment is not an integer or out of range")
	errIncrexIncrFloat     = errors.New("ERR Increment is not a valid float")
	errIncrexIncrInfinite  = errors.New("ERR BYFLOAT increment cannot be Infinity")
	errIncrexLowerFloat    = errors.New("ERR LBOUND is not a valid float")
	errIncrexUpperFloat    = errors.New("ERR UBOUND is not a valid float")
	errIncrexLowerInteger  = errors.New("ERR LBOUND is not an integer or out of range")
	errIncrexUpperInteger  = errors.New("ERR UBOUND is not an integer or out of range")
	errIncrexBounds        = errors.New("ERR LBOUND can't be greater than UBOUND")
	errIncrexENX           = errors.New("ERR ENX flag requires an expiration")
	errIncrexValueInfinite = errors.New("ERR value cannot be Infinity")
	errIncrexDeltaInfinite = errors.New("ERR applied increment would be Infinity")
	errIncrexDeltaOverflow = errors.New("ERR applied increment would overflow")
)

// digestLength is the length of a digest's text: XXH3's 64 bits in hex.
const digestLength = 16

// stringDigest is Redis's stringDigest: the XXH3 64-bit hash of a string,
// in 16 lower-case hexadecimal digits.
func stringDigest(value string) string {
	var buf [digestLength]byte
	h := xxh3.Hash(value)
	for i := digestLength - 1; i >= 0; i-- {
		buf[i] = "0123456789abcdef"[h&0xf]
		h >>= 4
	}
	return string(buf[:])
}

// digestEqual compares a digest with one a client gave, ignoring case, as
// Redis's strcasecmp does: ASCII letters only.
func digestEqual(digest, given string) bool {
	if len(digest) != len(given) {
		return false
	}
	for i := 0; i < len(given); i++ {
		c := given[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if digest[i] != c {
			return false
		}
	}
	return true
}

// opDigest is the digest of the string key holds, as DIGEST answers it, and
// false for none.
func (e *Engine) opDigest(key string) (string, bool) {
	value, ok := e.opGet(key)
	if !ok {
		return "", false
	}
	return stringDigest(value), true
}

// Conditions on the string a key holds, SET's and DELEX's: IFEQ and IFNE
// compare it with a value, IFDEQ and IFDNE its digest with a digest.
const (
	condNone uint8 = iota
	condEqual
	condNotEqual
	condDigestEqual
	condDigestNotEqual
)

// valueCondition is a condition and the value or digest it compares with.
type valueCondition struct {
	kind  uint8
	match string
}

// conditionKind reads a condition's name, Redis's IFEQ, IFNE, IFDEQ or
// IFDNE in any case, and answers condNone for anything else.
func conditionKind(name string) uint8 {
	switch {
	case equalFoldASCII(name, "IFEQ"):
		return condEqual
	case equalFoldASCII(name, "IFNE"):
		return condNotEqual
	case equalFoldASCII(name, "IFDEQ"):
		return condDigestEqual
	case equalFoldASCII(name, "IFDNE"):
		return condDigestNotEqual
	}
	return condNone
}

// equalFoldASCII is strcasecmp(s, upper) == 0 for an upper-case ASCII word.
func equalFoldASCII(s, upper string) bool {
	if len(s) != len(upper) {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'a' <= c && c <= 'z' {
			c -= 'a' - 'A'
		}
		if c != upper[i] {
			return false
		}
	}
	return true
}

// holds reports whether value meets the condition. A digest is checked for
// its length only when there is a value to compare it with, as Redis checks
// it.
func (c valueCondition) holds(value string) (bool, error) {
	switch c.kind {
	case condEqual:
		return value == c.match, nil
	case condNotEqual:
		return value != c.match, nil
	}
	if len(c.match) != digestLength {
		return false, errDigestLength
	}
	return digestEqual(stringDigest(value), c.match) == (c.kind == condDigestEqual), nil
}

// setConditionHolds reports whether SET's condition lets it write over what
// setTarget found, as setGenericCommand decides: IFEQ and IFDEQ need a value
// to compare and decline where there is none, IFNE and IFDNE write where
// there is none, and a key of another type is refused.
func (e *Engine) setConditionHolds(t stringTarget, c valueCondition) (bool, error) {
	if !t.exists() {
		return c.kind == condNotEqual || c.kind == condDigestNotEqual, nil
	}
	// Redis reads the value it compares with lookupKeyRead, a keyspace hit.
	e.noteLookup(true)
	if t.obj == nil {
		return false, errWrongType
	}
	return c.holds(t.obj.Value)
}

// opDelex deletes key as DELEX does and reports whether it did. With no
// condition it is DEL, of a key of any type. With one, a missing key is
// left, a key of another type is refused, then the condition's name is read
// - an unknown one refused only now, as Redis refuses it - and the key is
// deleted if the string it holds meets the condition. A deletion is recorded
// as the DEL it is.
func (e *Engine) opDelex(key string, condition, match string, conditional bool) (bool, error) {
	if !conditional {
		if e.space.DeleteAnywhere(key) {
			e.aofRecord("DEL", key)
			return true, nil
		}
		return false, nil
	}
	owner, held := e.space.OwnerOf(key)
	// A condition is read with lookupKeyRead, a keyspace hit or a miss.
	e.noteLookup(held)
	if !held {
		return false, nil
	}
	if owner.KeyspaceName() != e.dictStore.KeyspaceName() {
		return false, errDelexType
	}
	c := valueCondition{kind: conditionKind(condition), match: match}
	if c.kind == condNone {
		return false, errDelexCondition
	}
	value, _ := e.opGet(key)
	holds, err := c.holds(value)
	if err != nil || !holds {
		return false, err
	}
	e.deleteString(key)
	return true, nil
}

// msetexRequest is what an MSETEX asks for: NX or XX across all its keys,
// and the expiry or KEEPTTL each key is stored with.
type msetexRequest struct {
	nx, xx bool
	set    setRequest
}

// opMSetEx stores each value of pairs as MSETEX does, and reports whether it
// did: with NX none of the keys may exist, of any type, with XX all of them
// must, and otherwise nothing is stored. Each key is stored as SET stores
// one, replacing a key of another type, and recorded as SET and PEXPIREAT,
// all of them with the same instant.
func (e *Engine) opMSetEx(pairs []string, req msetexRequest) bool {
	if req.nx || req.xx {
		for i := 0; i < len(pairs); i += 2 {
			_, held := e.space.OwnerOf(pairs[i])
			if (req.nx && held) || (req.xx && !held) {
				return false
			}
		}
	}
	for i := 0; i < len(pairs); i += 2 {
		e.opSet(e.setTarget(pairs[i]), pairs[i], pairs[i+1], req.set)
	}
	return true
}

// increxRequest is what an INCREX asks for, as parseIncrExArgumentsOrReply
// reads it.
type increxRequest struct {
	// byFloat adds incrF in a long double, bounded by lowerF and upperF;
	// otherwise incr is added as an integer, bounded by lower and upper.
	byFloat               bool
	incr, lower, upper    int64
	incrF, lowerF, upperF *big.Float
	// saturate clamps a sum out of bounds to the bound; otherwise the
	// increment is refused and nothing changes.
	saturate bool
	// expires sets the expiry at, in Unix milliseconds, and with enx only
	// on a key that has none; persist removes it.
	expires, enx, persist bool
	at                    int64
}

// increxResult is what INCREX answers: the value after the increment and
// the increment applied, as integers or as long doubles' text. A refused
// increment answers the value as it was and 0, and changes nothing.
type increxResult struct {
	value, applied   int64
	valueF, appliedF string
	refused          bool
}

// ldblMax is LDBL_MAX, the largest finite x87 long double: 64 bits of ones
// times 2^16320.
var ldblMax = new(big.Float).SetMantExp(new(big.Float).SetPrec(longDoublePrec).SetUint64(math.MaxUint64), longDoubleMaxExp-longDoublePrec)

// increxSum works out what INCREX does to key, writing nothing, so that the
// command can admit its reply before it writes.
func (e *Engine) increxSum(key string, req increxRequest) (increxResult, error) {
	obj := e.dictStore.Get(key)
	if req.byFloat {
		old := new(big.Float).SetPrec(longDoublePrec)
		if obj != nil {
			var ok bool
			if old, ok = parseLongDouble(obj.Value); !ok {
				return increxResult{}, errNotAFloat
			}
			if old.IsInf() {
				return increxResult{}, errIncrexValueInfinite
			}
		}
		sum, finite := addLongDouble(old, req.incrF)
		if !finite || sum.Cmp(req.upperF) > 0 || sum.Cmp(req.lowerF) < 0 {
			if !req.saturate {
				return increxResult{valueF: formatLongDoubleHuman(old), appliedF: "0", refused: true}, nil
			}
			switch {
			case !finite && req.incrF.Sign() >= 0, finite && sum.Cmp(req.upperF) > 0:
				sum = req.upperF
			default:
				sum = req.lowerF
			}
		}
		applied, finite := subLongDouble(sum, old)
		if !finite {
			return increxResult{}, errIncrexDeltaInfinite
		}
		return increxResult{valueF: formatLongDoubleHuman(sum), appliedF: formatLongDoubleHuman(applied)}, nil
	}
	old := int64(0)
	if obj != nil {
		var valid bool
		if old, valid = canonicalInteger(obj.Value); !valid {
			return increxResult{}, errNotAnInteger
		}
	}
	sum, overflow := addOverflows(old, req.incr)
	if overflow || sum > req.upper || sum < req.lower {
		if !req.saturate {
			return increxResult{value: old, refused: true}, nil
		}
		switch {
		case overflow && req.incr >= 0, !overflow && sum > req.upper:
			sum = req.upper
		default:
			sum = req.lower
		}
	}
	applied, overflow := subOverflows(sum, old)
	if overflow {
		return increxResult{}, errIncrexDeltaOverflow
	}
	return increxResult{value: sum, applied: applied}, nil
}

// addOverflows and subOverflows are a + b and a - b, and whether the result
// overflowed 64 bits.
func addOverflows(a, b int64) (int64, bool) {
	sum := a + b
	return sum, (b > 0 && sum < a) || (b < 0 && sum > a)
}

func subOverflows(a, b int64) (int64, bool) {
	diff := a - b
	return diff, (b < 0 && diff < a) || (b > 0 && diff > a)
}

// storeIncrex applies what increxSum worked out: the value stored, the key
// keeping its expiry unless the request sets or removes it. An expiry
// already past deletes the key instead, recorded as DEL, and a refused
// increment changes nothing. The write is recorded as Redis propagates
// INCREX, as SET with the final value: plain under PERSIST, followed by
// PEXPIREAT under a new expiry, and with KEEPTTL otherwise.
func (e *Engine) storeIncrex(key string, req increxRequest, result increxResult) {
	if result.refused {
		return
	}
	exists := e.dictStore.Get(key) != nil
	_, hasExpiry := e.dictStore.GetExpiry(key)
	hasExpiry = exists && hasExpiry
	setExpiry := req.expires && (!req.enx || !hasExpiry)
	if setExpiry && req.at <= time.Now().UnixMilli() && !e.aof.replaying && !e.replicaApplying {
		if exists {
			e.deleteString(key)
		}
		return
	}
	value := result.valueF
	if !req.byFloat {
		value = strconv.FormatInt(result.value, 10)
	}
	if exists {
		e.dictStore.UpdateValue(key, value)
	} else {
		e.dictStore.Put(key, e.dictStore.NewObj(value))
	}
	switch {
	case req.persist:
		e.dictStore.ClearExpiry(key)
		e.aofRecord("SET", key, value)
	case setExpiry:
		e.dictStore.SetExpiryAt(key, uint64(req.at))
		e.aofRecord("SET", key, value)
		e.aofRecordInt("PEXPIREAT", key, req.at)
	default:
		e.aofRecord("SET", key, value, "KEEPTTL")
	}
}

// opIncrex is INCREX on key for a Go caller: what increxSum works out,
// applied.
func (e *Engine) opIncrex(key string, req increxRequest) (increxResult, error) {
	result, err := e.increxSum(key, req)
	if err == nil {
		e.storeIncrex(key, req, result)
	}
	return result, err
}
