package core

import (
	"errors"
	"strconv"
)

var errNotAnInteger = errors.New("ERR value is not an integer or out of range")

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
