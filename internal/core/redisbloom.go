package core

import (
	"errors"
	"math"
	"strconv"
	"strings"

	"github.com/brandopakel/keel/internal/data_structure"
)

// What the BF.* and CF.* commands share in following RedisBloom 8.10.1, the
// module Redis 8.10.1 ships: how it reads a number, how it finds an option, how
// it treats a key of another type, and its error text, word for word. A client
// written against RedisBloom gets the replies it was written for, the refusals
// included. scripts/redisbloom-parity.py compares the two servers byte for byte.

// The refusals the two filter families share, as RedisBloom words them. Some
// carry no ERR prefix, because RedisBloom sends none.
var (
	errFilterExists   = errors.New("ERR item exists")
	errFilterNotFound = errors.New("ERR not found")
)

// filterStatus is what RedisBloom finds when it opens the key a command names.
type filterStatus int

const (
	filterMissing filterStatus = iota
	filterHeld
	filterOtherType
)

// filterKeyStatus reports whether key holds a filter of own's type, nothing,
// or something else.
//
// The filter commands check a key's type themselves, at the point in each
// command RedisBloom does, rather than in checkKeyTypes before the command
// runs: RedisBloom reads a reservation's parameters before it opens the key, so
// a bad parameter is reported ahead of a key of the wrong type; and its reads
// answer "no" for a key of another type where every other command answers
// WRONGTYPE. Asking every keyspace, as checkKeyTypes does, also reaps an
// expired key of any type, as checkKeyTypes did.
func (e *Engine) filterKeyStatus(key string, own data_structure.Keyspace) filterStatus {
	owner, held := e.space.OwnerOf(key)
	switch {
	case !held:
		return filterMissing
	case owner.KeyspaceName() == own.KeyspaceName():
		return filterHeld
	}
	return filterOtherType
}

// moduleArgIndex is RMUtil_ArgIndex: the first argument equal to name, any
// case, or -1. RedisBloom looks for its options among every argument, the key
// and the numbers before them included, and so does this; a filter named
// "nonscaling" is a filter that does not grow, as it is in RedisBloom.
func moduleArgIndex(name string, args []string) int {
	for i, arg := range args {
		if strings.EqualFold(arg, name) {
			return i
		}
	}
	return -1
}

// longStrSize is Redis's LONG_STR_SIZE: an integer argument this long or
// longer is refused before it is read.
const longStrSize = 21

// redisInteger reads a signed 64-bit integer exactly as Redis's string2ll
// does, which is how RedisBloom reads every integer argument: an optional
// minus sign and decimal digits, with no plus sign, no leading zero, no space
// and no overflow. strconv is more forgiving - it reads "+5" and "007" - and
// RedisBloom refuses both.
func redisInteger(s string) (int64, bool) {
	if len(s) == 0 || len(s) >= longStrSize {
		return 0, false
	}
	if s == "0" {
		return 0, true
	}
	negative := s[0] == '-'
	digits := s
	if negative {
		digits = s[1:]
	}
	if len(digits) == 0 || digits[0] < '1' || digits[0] > '9' {
		return 0, false
	}
	var v uint64
	for i := 0; i < len(digits); i++ {
		c := digits[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		if v > math.MaxUint64/10 {
			return 0, false
		}
		v *= 10
		if v > math.MaxUint64-uint64(c-'0') {
			return 0, false
		}
		v += uint64(c - '0')
	}
	if negative {
		if v > 1<<63 {
			return 0, false
		}
		return -int64(v), true
	}
	if v > math.MaxInt64 {
		return 0, false
	}
	return int64(v), true
}

// redisDouble reads a double exactly as Redis's string2d does, which is how
// RedisBloom reads an error rate: Redis parses decimal text itself and hands
// anything else to the C library's strtod, then refuses an empty string, one
// that starts with a space, one with anything left over, an overflow, an
// underflow to zero, and NaN. Infinity is a number here; the rate's range
// check refuses it.
//
// strconv.ParseFloat agrees with that on everything but four points, which are
// handled here: it reads NaN, which Redis refuses; it returns zero for an
// underflow, which Redis refuses; it accepts underscores after a base prefix,
// which strtod does not; and it wants a binary exponent on a hexadecimal
// number, which strtod does not.
func redisDouble(s string) (float64, bool) {
	if s == "" || isCSpace(s[0]) || strings.IndexByte(s, '_') >= 0 {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil && isHexWithoutExponent(s) {
		f, err = strconv.ParseFloat(s+"p0", 64)
	}
	if err != nil || math.IsNaN(f) {
		return 0, false
	}
	if f == 0 && hasNonzeroMantissa(s) {
		return 0, false
	}
	return f, true
}

// isCSpace is C's isspace in the C locale.
func isCSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\v' || c == '\f' || c == '\r'
}

func hexMantissa(s string) (string, bool) {
	if s != "" && (s[0] == '+' || s[0] == '-') {
		s = s[1:]
	}
	if len(s) < 2 || s[0] != '0' || (s[1] != 'x' && s[1] != 'X') {
		return "", false
	}
	return s[2:], true
}

// isHexWithoutExponent is strtod's hexadecimal form with no p exponent:
// 0x, then hex digits with at most one point among them.
func isHexWithoutExponent(s string) bool {
	m, ok := hexMantissa(s)
	if !ok {
		return false
	}
	digits, points := 0, 0
	for i := 0; i < len(m); i++ {
		switch c := m[i]; {
		case c == '.':
			points++
		case (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F'):
			digits++
		default:
			return false
		}
	}
	return digits > 0 && points <= 1
}

// hasNonzeroMantissa reports whether a number that read as zero was written
// with a nonzero digit before its exponent - an underflow, which strtod
// reports as a range error and Redis refuses.
func hasNonzeroMantissa(s string) bool {
	if m, hex := hexMantissa(s); hex {
		for i := 0; i < len(m) && m[i] != 'p' && m[i] != 'P'; i++ {
			if c := m[i]; (c >= '1' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') {
				return true
			}
		}
		return false
	}
	for i := 0; i < len(s) && s[i] != 'e' && s[i] != 'E'; i++ {
		if s[i] >= '1' && s[i] <= '9' {
			return true
		}
	}
	return false
}

// replayingFilterLog is whether a filter command is being replayed from the
// log or applied from a primary rather than sent by a client.
//
// A log is replayed by the build that reads it, and the build before this one
// accepted, and recorded, reservations RedisBloom refuses - a cuckoo filter of
// capacity 1, a capacity written 007, an expansion past 32768 - and answered
// a CF.DEL of a missing key with 0, which is recorded too. Some it read
// differently from RedisBloom, too: RedisBloom finds its options among every
// argument, so to it BF.RESERVE nonscaling 0.01 100 is a filter that does not
// grow, and CF.RESERVE expansion 1000 an expansion of 1000. Refusing or
// rereading them now would stop that log loading, or load it without the
// filters it holds, so a replayed reservation the earlier build would have
// accepted is read as that build read it. What a client sends is held to
// RedisBloom's rules, and logged in a form that replays to the same filter.
func (e *Engine) replayingFilterLog() bool { return e.aof.replaying || replicaApplying }
