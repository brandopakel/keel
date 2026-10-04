package core

import (
	"errors"
	"io"

	"github.com/brandopakel/keel/internal/constant"
	"github.com/brandopakel/keel/internal/data_structure"
)

// Command dispatch.
//
// One table from name to handler. A command is registered here, in the type
// table in keytype.go so a name held by another type is refused, in
// commandArity in command_check.go so its arguments are counted before it
// runs, and - if it changes anything - in the log's writeCommands; each of
// those is a list that can be read against the others. Dispatch looks a
// command up in commands, the index of this table and commandArity together.
//
// The table is in two parts while the stores move into Engine (plan step
// 2.1): engineCommandTable holds the families that have moved, and this one
// the rest. A command is in exactly one of them.
var commandTable = map[string]func([]string) []byte{
	"PING": cmdPING, "ECHO": cmdECHO, "SELECT": cmdSELECT,
	"UNWATCH": cmdUNWATCH,

	// Strings
	"SET": cmdSET, "SETNX": cmdSETNX, "GET": cmdGET, "INCR": cmdINCR, "INCRBY": cmdINCRBY, "DECR": cmdDECR, "DECRBY": cmdDECRBY, "MGET": cmdMGET, "MSET": cmdMSET,
	"SETEX": cmdSETEX, "PSETEX": cmdPSETEX,
	"LCS": cmdLCS,

	// Keys and expiry
	"DEL": cmdDEL, "UNLINK": cmdUNLINK, "EXISTS": cmdEXISTS, "TYPE": cmdTYPE, "KEYS": cmdKEYS, "SCAN": cmdSCAN,
	"TTL": cmdTTL, "PTTL": cmdPTTL, "EXPIRE": cmdEXPIRE, "PEXPIREAT": cmdPEXPIREAT,
	"PEXPIRE": cmdPEXPIRE, "EXPIREAT": cmdEXPIREAT, "PERSIST": cmdPERSIST,

	// Server
	"KEEL.PROMOTE": cmdPROMOTE, "KEEL.FENCE": cmdFENCE,
	"KEEL.REPL.PULL":  cmdReplicationPull,
	"KEEL.REPL.PULL2": cmdReplicationPullV2,
	"DBSIZE":          cmdDBSIZE, "FLUSHDB": cmdFLUSHDB, "MEMORY": cmdMEMORY, "INFO": cmdINFO,
	"BGREWRITEAOF": cmdBGREWRITEAOF,
	"KEEL.DUMP":    cmdDUMP, "KEEL.RESTORE": cmdRESTORE,
	// The names from before the server was renamed, so a log written then
	// still replays; a command is written to the log under its current name.
	"MEMKV.DUMP": cmdDUMP, "MEMKV.RESTORE": cmdRESTORE,

	// Sorted sets, and the geospatial index built on them
	"ZCOUNT": cmdZCOUNT, "ZRANGEBYSCORE": cmdZRANGEBYSCORE, "ZREVRANGEBYSCORE": cmdZREVRANGEBYSCORE,
	"ZINCRBY": cmdZINCRBY, "ZPOPMIN": cmdZPOPMIN, "ZPOPMAX": cmdZPOPMAX,
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

// engineCommandTable is the part of the dispatch table whose handlers are
// Engine methods: the families whose stores have moved into the engine. Each
// runs on the engine that dispatches it and reads its store from there. A
// family moves here from commandTable when its store moves; once every
// handler has, the two are one table again.
var engineCommandTable = map[string]func(*Engine, []string) []byte{
	// Hashes
	"HSET": (*Engine).cmdHSET, "HSETNX": (*Engine).cmdHSETNX, "HGET": (*Engine).cmdHGET, "HMGET": (*Engine).cmdHMGET,
	"HDEL": (*Engine).cmdHDEL, "HEXISTS": (*Engine).cmdHEXISTS, "HLEN": (*Engine).cmdHLEN, "HKEYS": (*Engine).cmdHKEYS,
	"HVALS": (*Engine).cmdHVALS, "HGETALL": (*Engine).cmdHGETALL, "HINCRBY": (*Engine).cmdHINCRBY,

	// Lists
	"LPUSH": (*Engine).cmdLPUSH, "RPUSH": (*Engine).cmdRPUSH, "LPOP": (*Engine).cmdLPOP,
	"RPOP": (*Engine).cmdRPOP, "LTRIM": (*Engine).cmdLTRIM, "LLEN": (*Engine).cmdLLEN,
	"LINDEX": (*Engine).cmdLINDEX, "LSET": (*Engine).cmdLSET, "LRANGE": (*Engine).cmdLRANGE,

	// Sets
	"SADD": (*Engine).cmdSADD, "SREM": (*Engine).cmdSREM, "SCARD": (*Engine).cmdSCARD,
	"SMEMBERS": (*Engine).cmdSMEMBERS, "SISMEMBER": (*Engine).cmdSISMEMBER,
	"SMISMEMBER": (*Engine).cmdSMISMEMBER, "SPOP": (*Engine).cmdSPOP, "SRANDMEMBER": (*Engine).cmdSRANDMEMBER,
	// SRAND is what this server called SRANDMEMBER before it took the Redis name.
	"SRAND": (*Engine).cmdSRANDMEMBER,
}

// cmdPING answers PONG, or echoes the one argument it is given.
func cmdPING(args []string) []byte {
	switch len(args) {
	case 0:
		return Encode("PONG", true)
	case 1:
		return encodeBoundedString(args[0])
	}
	return Encode(wrongArguments("PING"), false)
}

// cmdECHO answers its one argument, as PING does when given one.
func cmdECHO(args []string) []byte {
	if len(args) != 1 {
		return Encode(wrongArguments("ECHO"), false)
	}
	return encodeBoundedString(args[0])
}

// cmdSELECT accepts database 0, the only one there is. Clients send SELECT when
// a connection URL names a database, and for 0 that is harmless. Any other
// number gets Redis's error for a database that does not exist rather than
// being mapped onto 0, which would mix the keys of applications that each
// believed they had a database to themselves.
func cmdSELECT(args []string) []byte {
	if len(args) != 1 {
		return Encode(wrongArguments("SELECT"), false)
	}
	n, valid := counterInteger(args[0])
	if !valid {
		return Encode(errNotAnInteger, false)
	}
	if n != 0 {
		return Encode(errors.New("ERR DB index is out of range"), false)
	}
	return constant.RespOk
}

// runningName is the name GEOSEARCH, the one command whose errors repeat the
// name it was sent as, was last sent as. It is set only for that command,
// before it runs, and read only while it runs.
var runningName string

// EvalAndResponse runs one command and writes its reply to c.
//
// The error it returns is the connection's, not the command's: a command that
// fails answers with a RESP error and returns nil here. The one exception is a
// command this server does not have, which is returned as an error so that a
// log replay stops on it rather than skipping past a command it cannot run.
func EvalAndResponse(cmd *Command, c io.ReadWriter) error {
	return defaultEngine.evalAndResponse(cmd, c)
}

// evalAndResponse is EvalAndResponse on e: the handlers that have moved into
// the engine run on e.
func (e *Engine) evalAndResponse(cmd *Command, c io.ReadWriter) error {
	// The reply is framed for the connection's protocol, held for exactly this
	// command - see resp3.go. Log replay and replica apply answer nobody, and
	// run as RESP2 whatever the command says: what they produce has to be the
	// same however the command first arrived.
	saved := replyRESP3
	replyRESP3 = cmd.RESP3 && !aof.replaying && !replicaApplying
	defer func() { replyRESP3 = saved }()

	// Redis names and counts a command before anything else, a replica's
	// refusal of a write included. A command this server does not have is
	// returned rather than answered, so that a log replay stops on it. A
	// well-formed command costs one comparison of its count here; anything
	// else is looked at in full.
	entry := commands[cmd.Cmd]
	if !entry.runs() {
		// Unknown, or one only the transport answers, with none here to.
		return unknownCommand(cmd)
	}
	var refused error
	if !entry.counted(len(cmd.Args)) {
		refused = commandRefusal(cmd, entry, true)
	}
	if refused == nil {
		refused = replicaCommandError(cmd.Cmd)
	}
	if entry.namesItself {
		runningName = cmd.sentName()
	}
	if refused != nil {
		_, err := c.Write(Encode(refused, false))
		return err
	}
	// Anything a command wants written to the log instead of itself is staged
	// while it runs, so the slate has to be clean before it starts. This comes
	// first because the type check below reads keys, and reading a key whose
	// expiry has passed reaps it - a removal that has to reach the log even
	// though the command it happened under went on to be refused.
	aofBegin(cmd.Cmd)
	defer aofEnd()

	// A name may only mean one thing at a time, and the stores cannot enforce
	// that individually because none of them knows about the others. Checked
	// before execution, so a refused command has not half-run.
	if err := checkKeyTypes(cmd); err != nil {
		res := Encode(err, false)
		aofCommit(cmd, res)
		_, werr := c.Write(res)
		return werr
	}

	suspended := data_structure.DefaultSpace.SuspendEviction
	data_structure.DefaultSpace.SuspendEviction = true
	var res []byte
	if entry.onEngine != nil {
		res = entry.onEngine(e, cmd.Args)
	} else {
		res = entry.run(cmd.Args)
	}
	// With eviction suspended, removals so far are lazy expiry. They precede
	// this command: recording them after INCR/HSET would delete the recreated key.
	// Recorded before the reply is written. FlushAOF runs between execution and
	// the write phase, so under appendfsync always the client hears "OK" only
	// once the log holding that OK is on disk.
	aofCommit(cmd, res)
	data_structure.DefaultSpace.SuspendEviction = suspended
	// The removal hook writes eviction DELs directly after the canonical body.
	data_structure.EnforceLimits()

	_, err := c.Write(res)
	return err
}
