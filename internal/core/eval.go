package core

import (
	"errors"
	"io"

	"github.com/brandopakel/keel/internal/constant"
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
// Every handler is an Engine method and runs on the engine dispatching it,
// reading that engine's stores and asking its space who holds a key.
var commandTable = map[string]func(*Engine, []string) []byte{
	"PING": (*Engine).cmdPING, "ECHO": (*Engine).cmdECHO, "SELECT": (*Engine).cmdSELECT,
	"UNWATCH": (*Engine).cmdUNWATCH,

	// Strings
	"SET": (*Engine).cmdSET, "SETNX": (*Engine).cmdSETNX, "GET": (*Engine).cmdGET, "INCR": (*Engine).cmdINCR,
	"INCRBY": (*Engine).cmdINCRBY, "DECR": (*Engine).cmdDECR, "DECRBY": (*Engine).cmdDECRBY,
	"MGET": (*Engine).cmdMGET, "MSET": (*Engine).cmdMSET, "SETEX": (*Engine).cmdSETEX,
	"PSETEX": (*Engine).cmdPSETEX, "LCS": (*Engine).cmdLCS,

	// Keys and expiry
	"DEL": (*Engine).cmdDEL, "UNLINK": (*Engine).cmdUNLINK, "EXISTS": (*Engine).cmdEXISTS,
	"TYPE": (*Engine).cmdTYPE, "KEYS": (*Engine).cmdKEYS, "SCAN": (*Engine).cmdSCAN, "TTL": (*Engine).cmdTTL,
	"PTTL": (*Engine).cmdPTTL, "EXPIRE": (*Engine).cmdEXPIRE, "PEXPIREAT": (*Engine).cmdPEXPIREAT,
	"PEXPIRE": (*Engine).cmdPEXPIRE, "EXPIREAT": (*Engine).cmdEXPIREAT, "PERSIST": (*Engine).cmdPERSIST,

	// Server
	"KEEL.PROMOTE": (*Engine).cmdPROMOTE, "KEEL.FENCE": (*Engine).cmdFENCE,
	"KEEL.REPL.PULL": (*Engine).cmdReplicationPull, "KEEL.REPL.PULL2": (*Engine).cmdReplicationPullV2,
	"DBSIZE": (*Engine).cmdDBSIZE, "FLUSHDB": (*Engine).cmdFLUSHDB, "MEMORY": (*Engine).cmdMEMORY,
	"INFO": (*Engine).cmdINFO, "BGREWRITEAOF": (*Engine).cmdBGREWRITEAOF, "KEEL.DUMP": (*Engine).cmdDUMP,
	"KEEL.RESTORE": (*Engine).cmdRESTORE,
	// The names from before the server was renamed, so a log written then
	// still replays; a command is written to the log under its current name.
	"MEMKV.DUMP": (*Engine).cmdDUMP, "MEMKV.RESTORE": (*Engine).cmdRESTORE,

	// Hashes
	"HSET": (*Engine).cmdHSET, "HSETNX": (*Engine).cmdHSETNX, "HGET": (*Engine).cmdHGET,
	"HMGET": (*Engine).cmdHMGET, "HDEL": (*Engine).cmdHDEL, "HEXISTS": (*Engine).cmdHEXISTS,
	"HLEN": (*Engine).cmdHLEN, "HKEYS": (*Engine).cmdHKEYS, "HVALS": (*Engine).cmdHVALS,
	"HGETALL": (*Engine).cmdHGETALL, "HINCRBY": (*Engine).cmdHINCRBY,

	// Lists
	"LPUSH": (*Engine).cmdLPUSH, "RPUSH": (*Engine).cmdRPUSH, "LPOP": (*Engine).cmdLPOP,
	"RPOP": (*Engine).cmdRPOP, "LTRIM": (*Engine).cmdLTRIM, "LLEN": (*Engine).cmdLLEN,
	"LINDEX": (*Engine).cmdLINDEX, "LSET": (*Engine).cmdLSET, "LRANGE": (*Engine).cmdLRANGE,

	// Sets
	"SADD": (*Engine).cmdSADD, "SREM": (*Engine).cmdSREM, "SCARD": (*Engine).cmdSCARD,
	"SMEMBERS": (*Engine).cmdSMEMBERS, "SISMEMBER": (*Engine).cmdSISMEMBER, "SMISMEMBER": (*Engine).cmdSMISMEMBER,
	"SPOP": (*Engine).cmdSPOP, "SRANDMEMBER": (*Engine).cmdSRANDMEMBER,
	// SRAND is what this server called SRANDMEMBER before it took the Redis name.
	"SRAND": (*Engine).cmdSRANDMEMBER,

	// Sorted sets, and the geospatial index built on them
	"ZCOUNT": (*Engine).cmdZCOUNT, "ZRANGEBYSCORE": (*Engine).cmdZRANGEBYSCORE,
	"ZREVRANGEBYSCORE": (*Engine).cmdZREVRANGEBYSCORE, "ZINCRBY": (*Engine).cmdZINCRBY,
	"ZPOPMIN": (*Engine).cmdZPOPMIN, "ZPOPMAX": (*Engine).cmdZPOPMAX, "ZRANGE": (*Engine).cmdZRANGE,
	"ZADD": (*Engine).cmdZADD, "ZRANK": (*Engine).cmdZRANK, "ZREM": (*Engine).cmdZREM,
	"ZSCORE": (*Engine).cmdZSCORE, "ZCARD": (*Engine).cmdZCARD, "GEOADD": (*Engine).cmdGEOADD,
	"GEODIST": (*Engine).cmdGEODIST, "GEOHASH": (*Engine).cmdGEOHASH, "GEOSEARCH": (*Engine).cmdGEOSEARCH,
	"GEOPOS": (*Engine).cmdGEOPOS,

	// Probabilistic structures
	"BF.RESERVE": (*Engine).cmdBFRESERVE, "BF.INFO": (*Engine).cmdBFINFO, "BF.ADD": (*Engine).cmdBFADD,
	"BF.MADD": (*Engine).cmdBFMADD, "BF.EXISTS": (*Engine).cmdBFEXISTS, "BF.MEXISTS": (*Engine).cmdBFMEXISTS,
	"CMS.INITBYDIM": (*Engine).cmdCMSINITBYDIM, "CMS.INITBYPROB": (*Engine).cmdCMSINITBYPROB,
	"CMS.INCRBY": (*Engine).cmdCMSINCRBY, "CMS.QUERY": (*Engine).cmdCMSQUERY,
	"MORRIS.INITBYDIM": (*Engine).cmdMORRISINITBYDIM, "MORRIS.INITBYPROB": (*Engine).cmdMORRISINITBYPROB,
	"MORRIS.INCRBY": (*Engine).cmdMORRISINCRBY, "MORRIS.QUERY": (*Engine).cmdMORRISQUERY,
	"MORRIS.INFO": (*Engine).cmdMORRISINFO, "PFADD": (*Engine).cmdPFADD, "PFCOUNT": (*Engine).cmdPFCOUNT,
	"PFMERGE": (*Engine).cmdPFMERGE, "CF.RESERVE": (*Engine).cmdCFRESERVE, "CF.ADD": (*Engine).cmdCFADD,
	"CF.ADDNX": (*Engine).cmdCFADDNX, "CF.EXISTS": (*Engine).cmdCFEXISTS, "CF.MEXISTS": (*Engine).cmdCFMEXISTS,
	"CF.DEL": (*Engine).cmdCFDEL, "CF.COUNT": (*Engine).cmdCFCOUNT, "CF.INFO": (*Engine).cmdCFINFO,
}

// cmdPING answers PONG, or echoes the one argument it is given.
func (e *Engine) cmdPING(args []string) []byte {
	switch len(args) {
	case 0:
		return e.encode("PONG", true)
	case 1:
		return e.encodeBoundedString(args[0])
	}
	return e.encode(wrongArguments("PING"), false)
}

// cmdECHO answers its one argument, as PING does when given one.
func (e *Engine) cmdECHO(args []string) []byte {
	if len(args) != 1 {
		return e.encode(wrongArguments("ECHO"), false)
	}
	return e.encodeBoundedString(args[0])
}

// cmdSELECT accepts database 0, the only one there is. Clients send SELECT when
// a connection URL names a database, and for 0 that is harmless. Any other
// number gets Redis's error for a database that does not exist rather than
// being mapped onto 0, which would mix the keys of applications that each
// believed they had a database to themselves.
func (e *Engine) cmdSELECT(args []string) []byte {
	if len(args) != 1 {
		return e.encode(wrongArguments("SELECT"), false)
	}
	n, valid := e.counterInteger(args[0])
	if !valid {
		return e.encode(errNotAnInteger, false)
	}
	if n != 0 {
		return e.encode(errors.New("ERR DB index is out of range"), false)
	}
	return constant.RespOk
}

// EvalAndResponse runs one command on e and writes its reply to c: the
// command runs in e's command scope, and its keys are type-checked, its
// eviction held off and its limits enforced on e's space.
//
// The error it returns is the connection's, not the command's: a command that
// fails answers with a RESP error and returns nil here. The one exception is a
// command this server does not have, which is returned as an error so that a
// log replay stops on it rather than skipping past a command it cannot run.
func (e *Engine) EvalAndResponse(cmd *Command, c io.ReadWriter) error {
	// The reply is framed for the connection's protocol, held on e for exactly
	// this command - see resp3.go. Log replay and replica apply answer nobody,
	// and run as RESP2 whatever the command says: what they produce has to be
	// the same however the command first arrived.
	saved := e.framing
	e.framing = framing{replyRESP3: cmd.RESP3 && !e.aof.replaying && !e.replicaApplying}
	defer func() { e.framing = saved }()

	// Redis names and counts a command before anything else, a replica's
	// refusal of a write included. A command this server does not have is
	// returned rather than answered, so that a log replay stops on it. A
	// well-formed command costs one comparison of its count here; anything
	// else is looked at in full.
	entry := commands[cmd.Cmd]
	if entry.run == nil {
		// Unknown, or one only the transport answers, with none here to.
		return unknownCommand(cmd)
	}
	var refused error
	if !entry.counted(len(cmd.Args)) {
		refused = commandRefusal(cmd, entry, true)
	}
	if refused == nil {
		refused = e.replicaCommandError(cmd.Cmd)
	}
	if entry.namesItself {
		e.runningName = cmd.sentName()
	}
	if refused != nil {
		_, err := c.Write(e.encode(refused, false))
		return err
	}
	// Anything a command wants written to the log instead of itself is staged
	// while it runs, so the slate has to be clean before it starts. This comes
	// first because the type check below reads keys, and reading a key whose
	// expiry has passed reaps it - a removal that has to reach the log even
	// though the command it happened under went on to be refused.
	//
	// aofEnd closes the scope after the reply is written, on each path from
	// here. It is called there rather than deferred: a deferred method call is
	// wrapped in a closure and called through it, an indirect call and a frame
	// on every command, where the deferred function of no arguments it was
	// while the log was package state was called directly. No panic is
	// recovered on the way out of a command, so the two do not differ.
	e.aofBegin(cmd.Cmd)

	// A name may only mean one thing at a time, and the stores cannot enforce
	// that individually because none of them knows about the others. Checked
	// before execution, so a refused command has not half-run.
	if err := e.checkKeyTypes(cmd, entry); err != nil {
		res := e.encode(err, false)
		e.aofCommit(cmd, res)
		_, werr := c.Write(res)
		e.aofEnd()
		return werr
	}

	suspended := e.space.SuspendEviction
	e.space.SuspendEviction = true
	res := entry.run(e, cmd.Args)
	// With eviction suspended, removals so far are lazy expiry. They precede
	// this command: recording them after INCR/HSET would delete the recreated key.
	// Recorded before the reply is written. FlushAOF runs between execution and
	// the write phase, so under appendfsync always the client hears "OK" only
	// once the log holding that OK is on disk.
	e.aofCommit(cmd, res)
	e.space.SuspendEviction = suspended
	// The removal hook writes eviction DELs directly after the canonical body.
	e.space.EnforceLimits()

	_, err := c.Write(res)
	e.aofEnd()
	return err
}
