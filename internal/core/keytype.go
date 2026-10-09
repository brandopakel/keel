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
func commandKeys(cmd *Command) []string { return keysBy(cmd, keyRuleOf(cmd.Cmd)) }

// keyRule is which of a command's arguments name keys.
type keyRule uint8

const (
	keyFirst    keyRule = iota // the first argument, as nearly every command has it
	keyFirstTwo                // LCS's two keys
	keyStride                  // every n-th argument, from the first: strideKeyCommands
	keyEvery                   // every argument: multiKeyCommands
)

// keyRuleOf is name's rule. Dispatch works it out once per command, in the
// index, so the type check reads it rather than looking the name up again.
func keyRuleOf(name string) keyRule {
	switch {
	case name == "LCS":
		return keyFirstTwo
	case strideKeyCommands[name] != 0:
		return keyStride
	case multiKeyCommands[name]:
		return keyEvery
	}
	return keyFirst
}

// keysBy returns the arguments of cmd that name keys under rule.
func keysBy(cmd *Command, rule keyRule) []string {
	if len(cmd.Args) == 0 {
		return nil
	}
	switch rule {
	case keyFirstTwo:
		return cmd.Args[:min(2, len(cmd.Args))]
	case keyStride:
		stride := strideKeyCommands[cmd.Cmd]
		keys := make([]string, 0, (len(cmd.Args)+stride-1)/stride)
		for i := 0; i < len(cmd.Args); i += stride {
			keys = append(keys, cmd.Args[i])
		}
		return keys
	case keyEvery:
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
func (e *Engine) typeError(cmd string, owner data_structure.Keyspace) error {
	switch cmd {
	case "LCS":
		return errLCSType
	case "PFADD", "PFCOUNT", "PFMERGE":
		if owner.KeyspaceName() == e.dictStore.KeyspaceName() {
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
var argumentsBeforeType = map[string]func(e *Engine, args []string) error{
	"INCRBY":  (*Engine).incrementArgument,
	"DECRBY":  (*Engine).decrementArgument,
	"HINCRBY": func(e *Engine, args []string) error { return e.integerArgument(args[2]) },
	"HSET": func(e *Engine, args []string) error {
		if len(args)%2 != 1 {
			return wrongArguments("HSET")
		}
		return nil
	},
	"LPOP":   func(e *Engine, args []string) error { return e.listPopArguments("LPOP", args) },
	"RPOP":   func(e *Engine, args []string) error { return e.listPopArguments("RPOP", args) },
	"LRANGE": (*Engine).rangeArgument, "LTRIM": (*Engine).rangeArgument,
	"SPOP": func(e *Engine, args []string) error {
		if len(args) > 2 {
			return errSyntax
		}
		return e.popCountArgument(args)
	},
	"SRANDMEMBER": (*Engine).randomCountArgument, "SRAND": (*Engine).randomCountArgument,
	"ZADD": func(_ *Engine, args []string) error { return zaddArguments(args) },
	"ZINCRBY": func(e *Engine, args []string) error {
		_, err := parseZScore(args[1])
		return err
	},
	"ZRANGE": func(e *Engine, args []string) error {
		_, err := e.parseZRange(args)
		return err
	},
	"ZRANGEBYSCORE": func(e *Engine, args []string) error {
		_, _, _, _, err := e.parseScoreRange(args, false)
		return err
	},
	"ZREVRANGEBYSCORE": func(e *Engine, args []string) error {
		_, _, _, _, err := e.parseScoreRange(args, true)
		return err
	},
	"ZCOUNT": func(e *Engine, args []string) error {
		_, err := parseScoreInterval(args[1], args[2])
		return err
	},
	"ZPOPMIN": func(e *Engine, args []string) error {
		_, err := e.zpopCount("ZPOPMIN", args)
		return err
	},
	"ZPOPMAX": func(e *Engine, args []string) error {
		_, err := e.zpopCount("ZPOPMAX", args)
		return err
	},
	"ZRANK": func(e *Engine, args []string) error {
		_, err := zrankArguments(args)
		return err
	},
	"GEOADD": func(_ *Engine, args []string) error { return geoaddArguments(args) },
	"GEODIST": func(e *Engine, args []string) error {
		_, err := geodistUnit(args)
		return err
	},
}

func (e *Engine) integerArgument(v string) error {
	if _, valid := e.counterInteger(v); !valid {
		return errNotAnInteger
	}
	return nil
}

func (e *Engine) incrementArgument(args []string) error { return e.integerArgument(args[1]) }

func (e *Engine) decrementArgument(args []string) error {
	n, valid := e.counterInteger(args[1])
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
func (e *Engine) listPopArguments(name string, args []string) error {
	if len(args) > 2 {
		return wrongArguments(name)
	}
	return e.popCountArgument(args)
}

func (e *Engine) popCountArgument(args []string) error {
	if len(args) == 2 {
		_, err := e.positiveCount(args[1])
		return err
	}
	return nil
}

func (e *Engine) rangeArgument(args []string) error {
	_, _, err := e.integerRange(args[1], args[2])
	return err
}

func (e *Engine) randomCountArgument(args []string) error {
	if len(args) > 2 {
		return errSyntax
	}
	_, _, err := e.randomCount(args)
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

// typedKeyspace is the type checkKeyTypes holds name's keys to, or "" for a
// command it passes over: one the type table does not constrain, a write that
// replaces whatever the name held, or a filter command, which checks its key
// itself.
func typedKeyspace(name string) string {
	space, checked := commandKeyspace[name]
	if !checked || replacingWrites[name] || filterCommands[name] {
		return ""
	}
	return space
}

// keyspaceReads are the commands that look their keys up to read them, as
// Redis's commands of the same names call lookupKeyRead and RedisBloom's open
// their keys with REDISMODULE_READ alone: each key looked up is a keyspace hit
// or a miss. The type check counts them for the typed ones, which look up
// every key they name, as Redis's do, in the same order and stopping at the
// same wrong type; the rest count where they look (MGET, EXISTS, TYPE, TTL,
// PTTL, SET's GET option, the filters' reads). Writes look their keys up to
// write them and count nothing, as Redis's lookupKeyWrite does, but for
// PFMERGE, which Redis reads every key of, its destination included.
//
// SRAND, MORRIS.QUERY, MORRIS.INFO and KEEL.DUMP have no Redis counterpart of
// their own name; they count as SRANDMEMBER, CMS.QUERY, CMS.INFO and DUMP do.
var keyspaceReads = map[string]bool{
	"GET": true, "LCS": true,
	"HGET": true, "HMGET": true, "HEXISTS": true, "HLEN": true, "HKEYS": true, "HVALS": true, "HGETALL": true,
	"LLEN": true, "LINDEX": true, "LRANGE": true,
	"SCARD": true, "SMEMBERS": true, "SISMEMBER": true, "SMISMEMBER": true, "SRANDMEMBER": true, "SRAND": true,
	"ZCOUNT": true, "ZRANGEBYSCORE": true, "ZREVRANGEBYSCORE": true, "ZRANGE": true, "ZRANK": true, "ZSCORE": true,
	"ZCARD":   true,
	"GEODIST": true, "GEOHASH": true, "GEOSEARCH": true, "GEOPOS": true,
	"PFCOUNT": true, "PFMERGE": true,
	"CMS.QUERY": true, "MORRIS.QUERY": true,
}

// checkKeyTypes reports an error if any key the command names is already held
// by a different kind of store: the command's own refusal of its arguments,
// where Redis reads those first, and otherwise Redis's refusal of the type.
//
// It reads the type and the key rule from entry, the command's index entry,
// which dispatch has already looked up: from the tables above they would be
// five lookups by name, made for every command whose keys are checked.
func (e *Engine) checkKeyTypes(cmd *Command, entry commandEntry) error {
	space := entry.typed
	if space == "" || len(cmd.Args) == 0 {
		return nil
	}

	keys := keysBy(cmd, entry.keys)
	for i, key := range keys {
		owner, held := e.space.OwnerOf(key)
		if entry.reads {
			e.noteLookup(held)
		}
		if held && owner.KeyspaceName() != space {
			if entry.reads && entry.keys == keyFirstTwo {
				// Redis's LCS looks both its keys up before it checks the
				// type of either.
				for _, rest := range keys[i+1:] {
					_, held := e.space.OwnerOf(rest)
					e.noteLookup(held)
				}
			}
			if arguments := argumentsBeforeType[cmd.Cmd]; arguments != nil {
				if err := arguments(e, cmd.Args); err != nil {
					return err
				}
			}
			return e.typeError(cmd.Cmd, owner)
		}
	}
	return nil
}
