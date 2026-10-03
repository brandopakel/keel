package core

import (
	"errors"
	"math"

	"github.com/brandopakel/keel/internal/data_structure"
)

// Type checking across the keyspaces.
//
// Each type has its own store, so a name was only unique within a type. SET k v
// and SADD k m both succeeded on the same name, GET and SMEMBERS both answered,
// and DEL k removed the string and left the set - so a name could mean two
// things at once and could not reliably be deleted. Redis answers WRONGTYPE
// here; there was nothing in this server able to.
//
// The check is one table and one place rather than a line at the top of forty
// commands. A line per command is the version that goes wrong: the next command
// added is the one that forgets it, and what it silently does instead is the
// bug this is fixing.
var commandKeyspace = map[string]string{
	"SET": "string", "SETNX": "string", "GET": "string", "INCR": "string", "INCRBY": "string", "DECR": "string", "DECRBY": "string", "MSET": "string",
	"SETEX": "string", "PSETEX": "string",
	"LCS": "string",

	// EXISTS, TYPE, KEYS, DEL, UNLINK and FLUSHDB are deliberately absent: they answer
	// about a name whatever type holds it, so constraining them to a keyspace
	// would make them refuse exactly the keys they exist to report on. MGET is
	// absent for a different reason - it answers nil for a key of another type,
	// which is Redis's rule and is explained where it is implemented.

	"HSET": "hash", "HSETNX": "hash", "HGET": "hash", "HMGET": "hash",
	"HDEL": "hash", "HEXISTS": "hash", "HLEN": "hash", "HKEYS": "hash",
	"HVALS": "hash", "HGETALL": "hash", "HINCRBY": "hash",

	"LPUSH": "list", "RPUSH": "list", "LPOP": "list", "RPOP": "list",
	"LTRIM": "list", "LLEN": "list", "LINDEX": "list", "LSET": "list", "LRANGE": "list",

	"SADD": "set", "SREM": "set", "SCARD": "set", "SMEMBERS": "set",
	"SISMEMBER": "set", "SMISMEMBER": "set", "SRANDMEMBER": "set", "SRAND": "set",
	"SPOP": "set",

	"ZADD": "zset", "ZRANK": "zset", "ZREM": "zset", "ZSCORE": "zset",
	"ZRANGE": "zset", "ZCARD": "zset",
	"ZCOUNT": "zset", "ZRANGEBYSCORE": "zset", "ZREVRANGEBYSCORE": "zset",
	"ZINCRBY": "zset", "ZPOPMIN": "zset", "ZPOPMAX": "zset",

	// Geospatial keys are sorted sets, as they are in Redis: the geohash is the
	// score, which is what makes GEOSEARCH a range query over a skip list.
	"GEOADD": "zset", "GEODIST": "zset", "GEOHASH": "zset",
	"GEOSEARCH": "zset", "GEOPOS": "zset",

	"BF.RESERVE": "bloom", "BF.INFO": "bloom", "BF.ADD": "bloom", "BF.MADD": "bloom",
	"BF.EXISTS": "bloom", "BF.MEXISTS": "bloom",

	"CMS.INITBYDIM": "cms", "CMS.INITBYPROB": "cms", "CMS.INCRBY": "cms",
	"CMS.QUERY": "cms",

	"MORRIS.INITBYDIM": "morris", "MORRIS.INITBYPROB": "morris",
	"MORRIS.INCRBY": "morris", "MORRIS.QUERY": "morris",

	"PFADD": "hll", "PFCOUNT": "hll", "PFMERGE": "hll",

	"CF.RESERVE": "cuckoo", "CF.ADD": "cuckoo", "CF.ADDNX": "cuckoo",
	"CF.EXISTS": "cuckoo", "CF.MEXISTS": "cuckoo", "CF.DEL": "cuckoo",
	"CF.COUNT": "cuckoo", "CF.INFO": "cuckoo",
}

// multiKeyCommands take a key in every argument rather than only the first.
var multiKeyCommands = map[string]bool{
	"PFCOUNT": true, "PFMERGE": true,
}

// strideKeyCommands take a key every n arguments, starting at the first. MSET
// is key value key value, so only the even positions name anything.
//
// Kept apart from multiKeyCommands rather than folded in as a stride of one,
// because the mistake to guard against is a command being added to the wrong
// one of these. A stride of two that should have been one checks half the keys
// it should; a list that says "all arguments" for MSET would type-check the
// values as though they were names, and refuse a perfectly good write the first
// time somebody stored a string whose contents happened to match a set's name.
var strideKeyCommands = map[string]int{
	"MSET": 2,
}

// commandKeys returns the arguments of cmd that name keys.
//
// One function rather than a rule repeated in each caller. The type check and
// the rewrite's dirty tracking have to agree about which arguments are keys,
// and the way they stop agreeing is one of them being taught about a new
// command and the other not.
func commandKeys(cmd *Command) []string {
	if len(cmd.Args) == 0 {
		return nil
	}
	if cmd.Cmd == "LCS" {
		return cmd.Args[:min(2, len(cmd.Args))]
	}
	if stride, ok := strideKeyCommands[cmd.Cmd]; ok {
		keys := make([]string, 0, (len(cmd.Args)+stride-1)/stride)
		for i := 0; i < len(cmd.Args); i += stride {
			keys = append(keys, cmd.Args[i])
		}
		return keys
	}
	if multiKeyCommands[cmd.Cmd] {
		return cmd.Args
	}
	return cmd.Args[:1]
}

// errWrongType is Redis's wording, so a client that already handles it from
// Redis handles it here.
var errWrongType = errors.New("WRONGTYPE Operation against a key holding the wrong kind of value")

// Two commands refuse a key of another type in words of their own, and so
// they do here. A HyperLogLog is a string in Redis, so a string key is one
// that does not hold a valid HyperLogLog; any other type is the plain
// WRONGTYPE.
var (
	errLCSType = errors.New("ERR The specified keys must contain string values")
	errNotHLL  = errors.New("WRONGTYPE Key is not a valid HyperLogLog string value.")
)

// typeError is the refusal of cmd for a key owner holds.
func typeError(cmd string, owner data_structure.Keyspace) error {
	switch cmd {
	case "LCS":
		return errLCSType
	case "PFADD", "PFCOUNT", "PFMERGE":
		if owner.KeyspaceName() == dictStore.KeyspaceName() {
			return errNotHLL
		}
	}
	return errWrongType
}

// argumentsBeforeType are the commands Redis reads some of the arguments of
// before it looks at the key, so a malformed argument is refused in its own
// words even when the key holds another type. Each returns the error the
// command's own reading of those arguments gives, from the same function the
// command reads them with, or nil. The type check asks only once it has found
// a key of another type: a command is never run against a key it should have
// refused, and a well-formed command pays nothing for it.
var argumentsBeforeType = map[string]func(args []string) error{
	"INCRBY":  incrementArgument,
	"DECRBY":  decrementArgument,
	"HINCRBY": func(args []string) error { return integerArgument(args[2]) },
	"HSET": func(args []string) error {
		if len(args)%2 != 1 {
			return wrongArguments("HSET")
		}
		return nil
	},
	"LPOP":   func(args []string) error { return listPopArguments("LPOP", args) },
	"RPOP":   func(args []string) error { return listPopArguments("RPOP", args) },
	"LRANGE": rangeArgument, "LTRIM": rangeArgument,
	"SPOP": func(args []string) error {
		if len(args) > 2 {
			return errSyntax
		}
		return popCountArgument(args)
	},
	"SRANDMEMBER": randomCountArgument, "SRAND": randomCountArgument,
	"ZADD": func(args []string) error {
		_, _, _, _, err := zaddArguments(args)
		return err
	},
	"ZINCRBY": func(args []string) error {
		_, err := parseZScore(args[1])
		return err
	},
	"ZRANGE": func(args []string) error {
		_, err := parseZRange(args)
		return err
	},
	"ZRANGEBYSCORE": func(args []string) error {
		_, _, _, _, err := parseScoreRange(args, false)
		return err
	},
	"ZREVRANGEBYSCORE": func(args []string) error {
		_, _, _, _, err := parseScoreRange(args, true)
		return err
	},
	"ZCOUNT": func(args []string) error {
		_, err := parseScoreInterval(args[1], args[2])
		return err
	},
	"ZPOPMIN": func(args []string) error {
		_, err := zpopCount("ZPOPMIN", args)
		return err
	},
	"ZPOPMAX": func(args []string) error {
		_, err := zpopCount("ZPOPMAX", args)
		return err
	},
	"ZRANK": func(args []string) error {
		_, err := zrankArguments(args)
		return err
	},
	"GEOADD": func(args []string) error {
		_, _, _, _, err := geoaddArguments(args)
		return err
	},
	"GEODIST": func(args []string) error {
		_, err := geodistUnit(args)
		return err
	},
}

func integerArgument(v string) error {
	if _, valid := counterInteger(v); !valid {
		return errNotAnInteger
	}
	return nil
}

func incrementArgument(args []string) error { return integerArgument(args[1]) }

func decrementArgument(args []string) error {
	n, valid := counterInteger(args[1])
	switch {
	case !valid:
		return errNotAnInteger
	case n == math.MinInt64:
		return errDecrOverflow
	}
	return nil
}

// listPopArguments is LPOP's and RPOP's reading: more than a count is the
// wrong number of arguments, then the count.
func listPopArguments(name string, args []string) error {
	if len(args) > 2 {
		return wrongArguments(name)
	}
	return popCountArgument(args)
}

func popCountArgument(args []string) error {
	if len(args) == 2 {
		_, err := positiveCount(args[1])
		return err
	}
	return nil
}

func rangeArgument(args []string) error {
	_, _, err := integerRange(args[1], args[2])
	return err
}

func randomCountArgument(args []string) error {
	if len(args) > 2 {
		return errSyntax
	}
	_, _, err := randomCount(args)
	return err
}

// writtenKeys returns the keys a command changes, for the rewrite that has to
// know which of its findings went stale.
//
// It reads the same table the type check uses, which is the point: one list of
// which argument is a key, consulted by everything that needs to know, rather
// than a second list to be kept in step with the first.
func writtenKeys(cmd *Command) []string {
	if len(cmd.Args) == 0 {
		return nil
	}
	switch cmd.Cmd {
	case "DEL", "UNLINK":
		// DEL names any number of keys and constrains none of them to a type,
		// so it is absent from the table below and handled here.
		return cmd.Args
	case "PFMERGE":
		// The destination is written and the sources are only read, but a
		// source recorded as dirty costs one redundant re-emit and a source
		// missed would be a bug, so all of them count.
		return cmd.Args
	case "KEEL.RESTORE", "MEMKV.RESTORE", "EXPIRE", "PEXPIRE", "EXPIREAT", "PEXPIREAT", "PERSIST":
		return cmd.Args[:1]
	}
	if _, known := commandKeyspace[cmd.Cmd]; known {
		return commandKeys(cmd)
	}
	return nil
}

// replacingWrites store a string whatever the name held before, as Redis's do:
// SET, SETEX, PSETEX and MSET replace a hash, list or set outright, and SETNX
// finds any existing key and leaves it. They stay in the table above for the
// keys they write and are exempt from the check below. Refusing them was this
// server's rule rather than Redis's - an application that reused a name for a
// different type, a summary string where a hash had been, failed here and
// nowhere else - and it came from the check being applied to every command
// alike, not from anything a string write needs. SET's GET option still answers
// WRONGTYPE for a key it cannot read as a string; cmdSET checks that itself.
var replacingWrites = map[string]bool{"SET": true, "SETEX": true, "PSETEX": true, "MSET": true, "SETNX": true}

// filterCommands check the type of the key they name themselves, at the point
// RedisBloom's do, and are exempt from the check below as well: a reservation
// reports a bad parameter ahead of a key of another type, and BF.EXISTS,
// CF.EXISTS and their kind answer no for one where every other read answers
// WRONGTYPE. They stay in the table above for the keys they write. See
// filterKeyStatus.
var filterCommands = map[string]bool{
	"BF.RESERVE": true, "BF.ADD": true, "BF.MADD": true, "BF.EXISTS": true, "BF.MEXISTS": true, "BF.INFO": true,
	"CF.RESERVE": true, "CF.ADD": true, "CF.ADDNX": true, "CF.EXISTS": true, "CF.MEXISTS": true,
	"CF.DEL": true, "CF.COUNT": true, "CF.INFO": true,
}

// checkKeyTypes reports an error if any key the command names is already held
// by a different kind of store: the command's own refusal of its arguments,
// where Redis reads those first, and otherwise Redis's refusal of the type.
func checkKeyTypes(cmd *Command) error {
	space, checked := commandKeyspace[cmd.Cmd]
	if !checked || len(cmd.Args) == 0 || replacingWrites[cmd.Cmd] || filterCommands[cmd.Cmd] {
		return nil
	}

	for _, key := range commandKeys(cmd) {
		if owner, held := data_structure.OwnerOf(key); held && owner.KeyspaceName() != space {
			if arguments := argumentsBeforeType[cmd.Cmd]; arguments != nil {
				if err := arguments(cmd.Args); err != nil {
					return err
				}
			}
			return typeError(cmd.Cmd, owner)
		}
	}
	return nil
}
