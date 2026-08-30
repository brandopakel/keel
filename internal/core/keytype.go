package core

import (
	"errors"

	"memkv/internal/data_structure"
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
	"SET": "string", "GET": "string", "TTL": "string", "EXPIRE": "string",
	"PEXPIREAT": "string", "INCR": "string",

	"SADD": "set", "SREM": "set", "SCARD": "set", "SMEMBERS": "set",
	"SISMEMBER": "set", "SMISMEMBER": "set", "SRAND": "set", "SPOP": "set",

	"ZADD": "zset", "ZRANK": "zset", "ZREM": "zset", "ZSCORE": "zset",
	"ZCARD": "zset",

	// Geospatial keys are sorted sets, as they are in Redis: the geohash is the
	// score, which is what makes GEOSEARCH a range query over a skip list.
	"GEOADD": "zset", "GEODIST": "zset", "GEOHASH": "zset",
	"GEOSEARCH": "zset", "GEOPOS": "zset",

	"BF.RESERVE": "bloom", "BF.INFO": "bloom", "BF.MADD": "bloom",
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

// errWrongType is Redis's wording, so a client that already handles it from
// Redis handles it here.
var errWrongType = errors.New("WRONGTYPE Operation against a key holding the wrong kind of value")

// checkKeyTypes reports an error if any key the command names is already held
// by a different kind of store.
//
// LCS is deliberately absent from the table: it reads two keys and treats a
// missing one as empty, which is what it should also do for a key of another
// type - see the comment on lcsValue.
func checkKeyTypes(cmd *MemKVCmd) error {
	space, checked := commandKeyspace[cmd.Cmd]
	if !checked || len(cmd.Args) == 0 {
		return nil
	}

	keys := cmd.Args[:1]
	if multiKeyCommands[cmd.Cmd] {
		keys = cmd.Args
	}
	for _, key := range keys {
		if owner, held := data_structure.OwnerOf(key); held && owner.KeyspaceName() != space {
			return errWrongType
		}
	}
	return nil
}
