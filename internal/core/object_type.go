package core

import (
	"errors"
	"strconv"

	"github.com/brandopakel/keel/internal/constant"
)

// The type and encoding byte a stored string carries; see constant.ObjTypeString.

func objType(te uint8) uint8     { return te & 0xf0 }
func objEncoding(te uint8) uint8 { return te & 0x0f }

var (
	errNotAnInteger = errors.New("ERR value is not an integer or out of range")
)

// assertType refuses an object of a type other than t. Every object in the
// string keyspace is a string, so this cannot fail today; it is what stops a
// second string type from being added without the commands noticing.
func assertType(te uint8, t uint8) error {
	if objType(te) != t {
		return errWrongType
	}
	return nil
}

// assertEncoding refuses an object not held in encoding e, with the error a
// client expects when it asks for arithmetic on something that is not a number.
func assertEncoding(te uint8, e uint8) error {
	if objEncoding(te) != e {
		return errNotAnInteger
	}
	return nil
}

// deduceTypeString decides how a value being stored as a string should be
// tagged. A value that is a canonical integer - one strconv would print the
// same way, so no leading zeros, plus sign or surrounding space - is tagged as
// one; Redis draws the line in the same place, which is why INCR on "007"
// is refused there and here.
func deduceTypeString(v string) (uint8, uint8) {
	if _, ok := canonicalInteger(v); ok {
		return constant.ObjTypeString, constant.ObjEncodingInt
	}
	return constant.ObjTypeString, constant.ObjEncodingRaw
}

func canonicalInteger(v string) (int64, bool) {
	// A canonical int64 is at most 20 bytes. ParseInt's error includes a copy
	// of its input, which needlessly copied every nonnumeric cached value.
	if v == "0" {
		return 0, true
	}
	if len(v) == 0 || len(v) > 20 {
		return 0, false
	}
	digits := v
	if v[0] == '-' {
		digits = v[1:]
	}
	if len(digits) == 0 || digits[0] < '1' || digits[0] > '9' {
		return 0, false
	}
	return parseDecimal([]byte(v))
}

// counterInteger preserves previously accepted AOF operations while enforcing
// canonical integer spelling on new requests. Older releases accepted +1/007
// counter arguments and noncanonical hash fields; refusing their historical
// commands during replay would make an otherwise valid upgrade fail.
func counterInteger(v string) (int64, bool) {
	if aof.replaying {
		n, err := strconv.ParseInt(v, 10, 64)
		return n, err == nil
	}
	return canonicalInteger(v)
}
