package core

import (
	"strconv"
	"strings"
)

// Redis's configuration directives, as config_specs.go declares them from
// Redis 8.10.1's src/config.c, and how a value given to CONFIG SET is read
// against one: Redis's set functions for each type (boolConfigSet,
// enumConfigSet, numericConfigSet, stringConfigSet), with their errors word
// for word, and CONFIG GET's way of writing the result.

// configKind is a directive's type, as Redis's table creates it.
type configKind uint8

const (
	configBool configKind = iota + 1
	configString
	configEnum
	configNumeric
	// configSpecial is a directive with set and get functions of its own
	// (save, dir, bind, ...), which config_keel.go gives Keel's.
	configSpecial
)

// configFlags are a directive's flags in Redis's table.
type configFlags uint16

const (
	configImmutable configFlags = 1 << iota
	configSensitive
	configDebug
	configMultiArg
	configHidden
	configProtected
	configDenyLoading
	configAlias
	configModule
	configVolatile
)

// configNumType is the C type a numeric directive is held in, which bounds
// how its value is checked (numericBoundaryCheck compares the unsigned ones
// unsigned) and what it reads back as.
type configNumType uint8

const (
	numInt configNumType = iota + 1
	numUInt
	numLong
	numULong
	numLongLong
	numULongLong
	numSizeT
	numSSizeT
	numTimeT
	numOffT
)

// configNumFlags say how a numeric directive's value is read: as a memory
// size (100mb), a percentage (N%), or an octal number. None, it is a plain
// integer.
type configNumFlags uint8

const (
	numMemory configNumFlags = 1 << iota
	numPercent
	numOctal
)

// configEnumValue is one name an enum directive takes, and its value.
type configEnumValue struct {
	name string
	val  int
}

// configSpec is one directive of Redis's table.
type configSpec struct {
	name, alias string
	kind        configKind
	flags       configFlags
	num         configNumType
	numFlags    configNumFlags
	// lower and upper are the bounds, held as Redis holds them, in a long
	// long; the unsigned types read them back unsigned.
	lower, upper int64
	enum         []configEnumValue
	// def is the default as CONFIG GET writes it; a special directive's is
	// config_keel.go's.
	def         string
	emptyIsNull bool
	// valid and apply name Redis's functions for a new value; condition is
	// the build condition the directive is declared under.
	valid, apply, condition string
}

// configLookupTable builds configLookup: every directive by its name and its
// alias, in lower case, as Redis's configs dictionary matches without regard
// to case. A directive declared under a build option Redis's default build
// leaves out (#ifdef LOG_REQ_RES, #ifdef USE_COMPRESSION) is not in it, as it
// is not in the Redis CI builds and Keel follows.
func configLookupTable() map[string]*configSpec {
	m := make(map[string]*configSpec, 2*len(redisConfigSpecs))
	for i := range redisConfigSpecs {
		s := &redisConfigSpecs[i]
		if !s.built() {
			continue
		}
		m[s.name] = s
		if s.alias != "" {
			m[s.alias] = s
		}
	}
	return m
}

// built reports whether Redis's default build declares s: one under an
// #ifdef of a build option it is not, and one under the #else of such an
// option, or no condition at all, is.
func (s *configSpec) built() bool { return !strings.HasPrefix(s.condition, "#ifdef ") }

// lookupConfig is Redis's lookupConfig: the directive name is, or its alias,
// in any case.
func lookupConfig(name string) *configSpec {
	configTables()
	if s, ok := configLookup[name]; ok {
		return s
	}
	return configLookup[strings.ToLower(name)]
}

// configArgs splits a multi-argument directive's value as Redis does, at
// every space (sdssplitlen), so that two spaces in a row give an empty
// argument and an empty value none at all.
func (s *configSpec) configArgs(value string) []string {
	if s.flags&configMultiArg == 0 {
		return []string{value}
	}
	if value == "" {
		return nil
	}
	return strings.Split(value, " ")
}

// read reads value as Redis's set function for s reads it, and returns it as
// CONFIG GET writes it, or Redis's error text. A special directive is read
// by config_keel.go, not here.
func (s *configSpec) read(value string) (string, string) {
	switch s.kind {
	case configBool:
		switch {
		case strings.EqualFold(value, "yes"):
			return "yes", ""
		case strings.EqualFold(value, "no"):
			return "no", ""
		}
		return "", "argument must be 'yes' or 'no'"
	case configString:
		return value, ""
	case configEnum:
		v, ok := s.enumValue(s.configArgs(value))
		if !ok {
			names := make([]string, len(s.enum))
			for i, e := range s.enum {
				names[i] = e.name
			}
			return "", "argument(s) must be one of the following: " + strings.Join(names, ", ")
		}
		return s.enumName(v), ""
	case configNumeric:
		n, err := s.readNumber(value)
		if err != "" {
			return "", err
		}
		return s.formatNumber(n), ""
	}
	return "", "unknown error"
}

// enumValue is configEnumGetValue: the value of one name, or for a
// multi-argument directive, the bits of every name, each matched without
// regard to case.
func (s *configSpec) enumValue(args []string) (int, bool) {
	bitflags := s.flags&configMultiArg != 0
	if len(args) == 0 || (!bitflags && len(args) != 1) {
		return 0, false
	}
	values := 0
	for _, arg := range args {
		matched := false
		for _, e := range s.enum {
			if strings.EqualFold(arg, e.name) {
				values |= e.val
				matched = true
			}
		}
		if !matched {
			return 0, false
		}
	}
	return values, true
}

// enumName is configEnumGetName: the first name with exactly this value or,
// for a multi-argument directive, the names whose bits make it up.
func (s *configSpec) enumName(v int) string {
	bitflags := s.flags&configMultiArg != 0
	var names []string
	unmatched := v
	for _, e := range s.enum {
		if e.val == v {
			return e.name
		}
		if bitflags && e.val != 0 && e.val == unmatched&e.val {
			names = append(names, e.name)
			unmatched &^= e.val
		}
	}
	if len(names) == 0 || unmatched != 0 {
		return "unknown"
	}
	return strings.Join(names, " ")
}

// readNumber is numericParseString and numericBoundaryCheck: the value as a
// long long, checked against s's bounds, with Redis's errors.
func (s *configSpec) readNumber(value string) (int64, string) {
	n, ok := int64(0), false
	if s.numFlags&numMemory != 0 {
		var u uint64
		if u, ok = memtoull(value); ok {
			n = int64(u)
		}
	}
	if !ok && s.numFlags&numPercent != 0 && len(value) > 1 && value[len(value)-1] == '%' {
		if p, pok := redisInteger(value[:len(value)-1]); pok && p >= 0 {
			// A percentage is held negative.
			n, ok = -p, true
		}
	}
	if !ok && s.numFlags&numOctal != 0 {
		if u, err := strconv.ParseInt(value, 8, 64); err == nil {
			n, ok = u, true
		}
	}
	if !ok && s.numFlags == 0 {
		n, ok = redisInteger(value)
	}
	if !ok {
		switch {
		case s.numFlags&numMemory != 0 && s.numFlags&numPercent != 0:
			return 0, "argument must be a memory or percent value"
		case s.numFlags&numMemory != 0:
			return 0, "argument must be a memory value"
		case s.numFlags&numOctal != 0:
			return 0, "argument couldn't be parsed as an octal number"
		}
		return 0, "argument couldn't be parsed into an integer"
	}
	switch s.num {
	case numULongLong, numUInt, numSizeT:
		u, lo, hi := uint64(n), uint64(s.lower), uint64(s.upper)
		if u > hi || u < lo {
			if s.numFlags&numOctal != 0 {
				return 0, "argument must be between " + strconv.FormatUint(lo, 8) + " and " + strconv.FormatUint(hi, 8) + " inclusive"
			}
			return 0, "argument must be between " + strconv.FormatUint(lo, 10) + " and " + strconv.FormatUint(hi, 10) + " inclusive"
		}
	default:
		if s.numFlags&numPercent != 0 && n < 0 {
			if n < s.lower {
				return 0, "percentage argument must be less or equal to " + strconv.FormatInt(-s.lower, 10)
			}
		} else if n > s.upper || n < s.lower {
			return 0, "argument must be between " + strconv.FormatInt(s.lower, 10) + " and " + strconv.FormatInt(s.upper, 10) + " inclusive"
		}
	}
	return s.held(n), ""
}

// held is n as s's C type holds it (setNumericType), read back as a long
// long (GET_NUMERIC_TYPE).
func (s *configSpec) held(n int64) int64 {
	switch s.num {
	case numInt:
		return int64(int32(n))
	case numUInt:
		return int64(uint32(n))
	}
	return n
}

// formatNumber is numericConfigGet: a percentage as N%, a memory size as an
// unsigned number, an octal number in octal, anything else in decimal.
func (s *configSpec) formatNumber(n int64) string {
	switch {
	case s.numFlags&numPercent != 0 && n < 0:
		return strconv.FormatInt(-n, 10) + "%"
	case s.numFlags&numMemory != 0:
		return strconv.FormatUint(uint64(n), 10)
	case s.numFlags&numOctal != 0:
		return strconv.FormatUint(uint64(n), 8)
	}
	return strconv.FormatInt(n, 10)
}

// memtoull is Redis's memtoull (util.c) as it reads on Linux, where Redis's
// CI runs: digits and an optional unit (b, k, kb, m, mb, g, gb, in any case;
// k, m and g are powers of 1000, kb, mb and gb of 1024). No sign, no space;
// no digits at all read as 0, as glibc's strtoull reads them; a number too
// large for 64 bits reads as the largest, as strtoull's ERANGE does, and so
// does a product that would overflow.
func memtoull(s string) (uint64, bool) {
	if strings.HasPrefix(s, "-") {
		return 0, false
	}
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	var mul uint64
	switch strings.ToLower(s[i:]) {
	case "", "b":
		mul = 1
	case "k":
		mul = 1000
	case "kb":
		mul = 1024
	case "m":
		mul = 1000 * 1000
	case "mb":
		mul = 1024 * 1024
	case "g":
		mul = 1000 * 1000 * 1000
	case "gb":
		mul = 1024 * 1024 * 1024
	default:
		return 0, false
	}
	if i >= 128 {
		return 0, false
	}
	var v uint64
	for _, c := range s[:i] {
		d := uint64(c - '0')
		if v > (^uint64(0)-d)/10 {
			v = ^uint64(0)
			break
		}
		v = v*10 + d
	}
	if v > ^uint64(0)/mul {
		return ^uint64(0), true
	}
	return v * mul, true
}
