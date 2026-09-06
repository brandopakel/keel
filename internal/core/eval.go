package core

import (
	"errors"
	"fmt"
	"github.com/brandopakel/keel/internal/data_structure"
	"io"
)

// Command dispatch.
//
// One table from name to handler. A command is registered here, in the type
// table in keytype.go so a name held by another type is refused, and - if it
// changes anything - in the log's writeCommands; each of those is a list that
// can be read against the others.
var commandTable = map[string]func([]string) []byte{
	"PING": cmdPING,

	// Strings
	"SET": cmdSET, "GET": cmdGET, "INCR": cmdINCR, "INCRBY": cmdINCRBY, "DECR": cmdDECR, "DECRBY": cmdDECRBY, "MGET": cmdMGET, "MSET": cmdMSET,
	"SETEX": cmdSETEX, "PSETEX": cmdPSETEX,
	"LCS": cmdLCS,

	// Keys and expiry
	"DEL": cmdDEL, "EXISTS": cmdEXISTS, "TYPE": cmdTYPE, "KEYS": cmdKEYS,
	"TTL": cmdTTL, "PTTL": cmdPTTL, "EXPIRE": cmdEXPIRE, "PEXPIREAT": cmdPEXPIREAT,
	"PEXPIRE": cmdPEXPIRE, "EXPIREAT": cmdEXPIREAT, "PERSIST": cmdPERSIST,

	// Server
	"KEEL.REPL.PULL": cmdReplicationPull,
	"DBSIZE":         cmdDBSIZE, "FLUSHDB": cmdFLUSHDB, "MEMORY": cmdMEMORY, "INFO": cmdINFO,
	"BGREWRITEAOF": cmdBGREWRITEAOF,
	"KEEL.DUMP":    cmdDUMP, "KEEL.RESTORE": cmdRESTORE,
	// The names from before the server was renamed, so a log written then
	// still replays; a command is written to the log under its current name.
	"MEMKV.DUMP": cmdDUMP, "MEMKV.RESTORE": cmdRESTORE,

	// Hashes
	"HSET": cmdHSET, "HSETNX": cmdHSETNX, "HGET": cmdHGET, "HMGET": cmdHMGET,
	"HDEL": cmdHDEL, "HEXISTS": cmdHEXISTS, "HLEN": cmdHLEN, "HKEYS": cmdHKEYS,
	"HVALS": cmdHVALS, "HGETALL": cmdHGETALL, "HINCRBY": cmdHINCRBY,

	// Lists
	"LPUSH": cmdLPUSH, "RPUSH": cmdRPUSH, "LPOP": cmdLPOP, "RPOP": cmdRPOP,
	"LTRIM": cmdLTRIM, "LLEN": cmdLLEN, "LINDEX": cmdLINDEX, "LSET": cmdLSET, "LRANGE": cmdLRANGE,

	// Sets
	"SADD": cmdSADD, "SREM": cmdSREM, "SCARD": cmdSCARD, "SMEMBERS": cmdSMEMBERS,
	"SISMEMBER": cmdSISMEMBER, "SMISMEMBER": cmdSMISMEMBER, "SPOP": cmdSPOP,
	"SRANDMEMBER": cmdSRANDMEMBER,
	// SRAND is what this server called SRANDMEMBER before it took the Redis name.
	"SRAND": cmdSRANDMEMBER,

	// Sorted sets, and the geospatial index built on them
	"ZRANGE": cmdZRANGE, "ZADD": cmdZADD, "ZRANK": cmdZRANK, "ZREM": cmdZREM, "ZSCORE": cmdZSCORE, "ZCARD": cmdZCARD,
	"GEOADD": cmdGEOADD, "GEODIST": cmdGEODIST, "GEOHASH": cmdGEOHASH,
	"GEOSEARCH": cmdGEOSEARCH, "GEOPOS": cmdGEOPOS,

	// Probabilistic structures
	"BF.RESERVE": cmdBFRESERVE, "BF.INFO": cmdBFINFO, "BF.ADD": cmdBFADD,
	"BF.MADD": cmdBFMADD, "BF.EXISTS": cmdBFEXISTS, "BF.MEXISTS": cmdBFMEXISTS,
	"CMS.INITBYDIM": cmdCMSINITBYDIM, "CMS.INITBYPROB": cmdCMSINITBYPROB,
	"CMS.INCRBY": cmdCMSINCRBY, "CMS.QUERY": cmdCMSQUERY,
	"MORRIS.INITBYDIM": cmdMORRISINITBYDIM, "MORRIS.INITBYPROB": cmdMORRISINITBYPROB,
	"MORRIS.INCRBY": cmdMORRISINCRBY, "MORRIS.QUERY": cmdMORRISQUERY, "MORRIS.INFO": cmdMORRISINFO,
	"PFADD": cmdPFADD, "PFCOUNT": cmdPFCOUNT, "PFMERGE": cmdPFMERGE,
	"CF.RESERVE": cmdCFRESERVE, "CF.ADD": cmdCFADD, "CF.ADDNX": cmdCFADDNX,
	"CF.EXISTS": cmdCFEXISTS, "CF.MEXISTS": cmdCFMEXISTS, "CF.DEL": cmdCFDEL,
	"CF.COUNT": cmdCFCOUNT, "CF.INFO": cmdCFINFO,
}

// cmdPING answers PONG, or echoes the one argument it is given.
func cmdPING(args []string) []byte {
	switch len(args) {
	case 0:
		return Encode("PONG", true)
	case 1:
		return Encode(args[0], false)
	}
	return Encode(errors.New("ERR wrong number of arguments for 'PING' command"), false)
}

// EvalAndResponse runs one command and writes its reply to c.
//
// The error it returns is the connection's, not the command's: a command that
// fails answers with a RESP error and returns nil here. The one exception is a
// command this server does not have, which is returned as an error so that a
// log replay stops on it rather than skipping past a command it cannot run.
func EvalAndResponse(cmd *Command, c io.ReadWriter) error {
	if err := replicaCommandError(cmd.Cmd); err != nil {
		_, werr := c.Write(Encode(err, false))
		return werr
	}
	// Anything a command wants written to the log instead of itself is staged
	// while it runs, so the slate has to be clean before it starts. This comes
	// first because the type check below reads keys, and reading a key whose
	// expiry has passed reaps it - a removal that has to reach the log even
	// though the command it happened under went on to be refused.
	aofBegin()

	// A name may only mean one thing at a time, and the stores cannot enforce
	// that individually because none of them knows about the others. Checked
	// before execution, so a refused command has not half-run.
	if err := checkKeyTypes(cmd); err != nil {
		res := Encode(err, false)
		aofCommit(cmd, res)
		_, werr := c.Write(res)
		return werr
	}

	handler, known := commandTable[cmd.Cmd]
	if !known {
		// Nothing ran, but the type check may still have reaped an expired
		// key, and that removal has to be recorded.
		aofCommit(cmd, nil)
		return fmt.Errorf("ERR unknown command '%s'", cmd.Cmd)
	}
	suspended := data_structure.SuspendEviction
	data_structure.SuspendEviction = true
	res := handler(cmd.Args)
	// With eviction suspended, removals so far are lazy expiry. They precede
	// this command: recording them after INCR/HSET would delete the recreated key.
	aofCommitExtras()
	data_structure.SuspendEviction = suspended
	data_structure.EnforceLimits()

	// Recorded before the reply is written. FlushAOF runs between execution and
	// the write phase, so under appendfsync always the client hears "OK" only
	// once the log holding that OK is on disk.
	aofCommit(cmd, res)

	_, err := c.Write(res)
	return err
}
