package core

import (
	"math/big"
	"strings"
)

// INCRBYFLOAT's arithmetic, as Redis 8.10.1 does it on the platform its
// replies are compared on, Linux x86-64: in C's long double, which there is
// the x87 80-bit extended format, a 64-bit mantissa (t_string.c,
// incrbyfloatCommand; util.c, string2ld and ld2string). The difference shows
// in the reply: 10.5 plus 0.1 is 10.6 in a 64-bit mantissa, printed with
// %.17Lf and its trailing zeros removed, and 10.59999999999999964 in a
// float64. A big.Float of 64 bits' precision, rounding to nearest even, adds
// as the x87 unit does, and prints the exact decimal expansion glibc's printf
// rounds from.
//
// What it does not model: the gradual underflow of values below the
// smallest normal long double, about 3.4e-4932, which no INCRBYFLOAT of
// ordinary numbers reaches.

// longDoublePrec is the long double's mantissa: 64 bits.
const longDoublePrec = 64

// longDoubleMaxExp is the binary exponent past which a long double
// overflows: LDBL_MAX is just under 2^16384, which big.Float's MantExp
// reports as an exponent of 16384 for a mantissa in [0.5, 1).
const longDoubleMaxExp = 16384

// longDoubleMinExp is the exponent below which a value is smaller than the
// smallest long double, the subnormal 2^-16445, and strtold underflows to
// zero.
const longDoubleMinExp = -16444

// maxLongDoubleChars is Redis's MAX_LONG_DOUBLE_CHARS: a number this long or
// longer is refused before it is read.
const maxLongDoubleChars = 5 * 1024

// parseLongDouble reads s as Redis's string2ld does, through strtold: an
// optional sign, then a decimal number with an optional exponent, a
// hexadecimal one (0x, with an optional binary exponent p), or inf or
// infinity. It refuses an empty string, leading space, anything after the
// number, nan, and a number too large or too small for a long double, which
// strtold reports as a range error. inf is accepted, as strtold accepts it:
// the sum is then refused as infinite.
func parseLongDouble(s string) (*big.Float, bool) {
	if len(s) == 0 || len(s) >= maxLongDoubleChars {
		return nil, false
	}
	body, negative := s, false
	switch body[0] {
	case '+':
		body = body[1:]
	case '-':
		body, negative = body[1:], true
	}
	lower := strings.ToLower(body)
	if lower == "inf" || lower == "infinity" {
		return new(big.Float).SetPrec(longDoublePrec).SetInf(negative), true
	}
	if !strtoldNumber(lower) {
		return nil, false
	}
	f, _, err := new(big.Float).SetPrec(longDoublePrec).SetMode(big.ToNearestEven).Parse(s, 0)
	// big.Float reads an exponent past its own range, 1e999999999, as an
	// infinity, and 1e-999999999 as zero, where strtold reports both as range
	// errors.
	if err != nil || f.IsInf() {
		return nil, false
	}
	if f.Sign() == 0 {
		return f, !nonzeroMantissa(lower)
	}
	if exp := f.MantExp(nil); exp > longDoubleMaxExp || exp < longDoubleMinExp {
		return nil, false
	}
	return f, true
}

// nonzeroMantissa reports whether a number strtoldNumber accepted has a
// digit other than zero before its exponent: whether a zero read from it is
// an underflow.
func nonzeroMantissa(s string) bool {
	marker := byte('e')
	if strings.HasPrefix(s, "0x") {
		s, marker = s[2:], 'p'
	}
	for i := 0; i < len(s) && s[i] != marker; i++ {
		if s[i] != '0' && s[i] != '.' {
			return true
		}
	}
	return false
}

// strtoldNumber reports whether s, lower-cased and without its sign, is a
// whole number as strtold reads one: decimal digits with an optional point
// and exponent, or 0x hexadecimal digits with an optional point and binary
// exponent. big.Float's own parser takes more (underscores, 0b and 0o), so
// the grammar is checked here first.
func strtoldNumber(s string) bool {
	digits := func(s string, hex bool) int {
		n := 0
		for n < len(s) {
			c := s[n]
			if c >= '0' && c <= '9' || hex && c >= 'a' && c <= 'f' {
				n++
				continue
			}
			break
		}
		return n
	}
	hex := strings.HasPrefix(s, "0x")
	if hex {
		s = s[2:]
	}
	whole := digits(s, hex)
	s = s[whole:]
	fraction := 0
	if strings.HasPrefix(s, ".") {
		fraction = digits(s[1:], hex)
		s = s[1+fraction:]
	}
	if whole+fraction == 0 {
		return false
	}
	marker := byte('e')
	if hex {
		marker = 'p'
	}
	if len(s) > 0 && s[0] == marker {
		s = s[1:]
		if len(s) > 0 && (s[0] == '+' || s[0] == '-') {
			s = s[1:]
		}
		n := digits(s, false)
		if n == 0 {
			return false
		}
		s = s[n:]
	}
	return len(s) == 0
}

// addLongDouble is a + b in a long double, and whether the sum is finite.
func addLongDouble(a, b *big.Float) (*big.Float, bool) {
	if a.IsInf() || b.IsInf() {
		// inf plus anything is inf or, with -inf, nan: neither is finite.
		return nil, false
	}
	sum := new(big.Float).SetPrec(longDoublePrec).SetMode(big.ToNearestEven).Add(a, b)
	if sum.Sign() != 0 && sum.MantExp(nil) > longDoubleMaxExp {
		return nil, false
	}
	return sum, true
}

// layoutPadLongDouble is layout padding for a throwaway control build: never run.
//
//go:noinline
func layoutPadLongDouble(s *[64]uint64) {
	s[0] += s[1] ^ 0x0
	s[7] += s[14] ^ 0x9e3779b1
	s[14] += s[27] ^ 0x3c6ef362
	s[21] += s[40] ^ 0xdaa66d13
	s[28] += s[53] ^ 0x78dde6c4
	s[35] += s[2] ^ 0x17156075
	s[42] += s[15] ^ 0xb54cda26
	s[49] += s[28] ^ 0x538453d7
	s[56] += s[41] ^ 0xf1bbcd88
	s[63] += s[54] ^ 0x8ff34739
	s[6] += s[3] ^ 0x2e2ac0ea
	s[13] += s[16] ^ 0xcc623a9b
}

// formatLongDoubleHuman writes f as Redis's ld2string does in its human mode:
// %.17Lf, then the trailing zeros after the point removed, and the point if
// nothing follows it, and -0 as 0.
func formatLongDoubleHuman(f *big.Float) string {
	text := f.Text('f', 17)
	if strings.IndexByte(text, '.') >= 0 {
		text = strings.TrimRight(text, "0")
		text = strings.TrimSuffix(text, ".")
	}
	if text == "-0" {
		return "0"
	}
	return text
}
