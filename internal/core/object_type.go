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
	if n, err := strconv.ParseInt(v, 10, 64); err == nil && strconv.FormatInt(n, 10) == v {
		return constant.ObjTypeString, constant.ObjEncodingInt
	}
	return constant.ObjTypeString, constant.ObjEncodingRaw
}
