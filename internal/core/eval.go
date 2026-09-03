package core

import (
	"errors"
	"fmt"
	"io"
)

func cmdPING(args []string) []byte {
	var buf []byte

	if len(args) > 1 {
		return Encode(errors.New("ERR wrong number of arguments for 'PING' command"), false)
	}

	if len(args) == 0 {
		buf = Encode("PONG", true)
	} else {
		buf = Encode(args[0], false)
	}

	return buf
}

func EvalAndResponse(cmd *MemKVCmd, c io.ReadWriter) error {
	var res []byte

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
		res = Encode(err, false)
		aofCommit(cmd, res)
		_, werr := c.Write(res)
		return werr
	}

	switch cmd.Cmd {
	case "PING":
		res = cmdPING(cmd.Args)
	case "SET":
		res = cmdSET(cmd.Args)
	case "GET":
		res = cmdGET(cmd.Args)
	case "TTL":
		res = cmdTTL(cmd.Args)
	case "DEL":
		res = cmdDEL(cmd.Args)
	case "EXPIRE":
		res = cmdEXPIRE(cmd.Args)
	case "PEXPIREAT":
		res = cmdPEXPIREAT(cmd.Args)
	case "INCR":
		res = cmdINCR(cmd.Args)
	case "LCS":
		res = cmdLCS(cmd.Args)
	case "EXISTS":
		res = cmdEXISTS(cmd.Args)
	case "TYPE":
		res = cmdTYPE(cmd.Args)
	case "KEYS":
		res = cmdKEYS(cmd.Args)
	case "MGET":
		res = cmdMGET(cmd.Args)
	case "MSET":
		res = cmdMSET(cmd.Args)
	case "FLUSHDB":
		res = cmdFLUSHDB(cmd.Args)
	case "DBSIZE":
		res = cmdDBSIZE(cmd.Args)
	case "MEMORY":
		res = cmdMEMORY(cmd.Args)
	case "INFO":
		res = cmdINFO(cmd.Args)
	case "BGREWRITEAOF":
		res = cmdBGREWRITEAOF(cmd.Args)
	case "MEMKV.DUMP":
		res = cmdMEMKVDUMP(cmd.Args)
	case "MEMKV.RESTORE":
		res = cmdMEMKVRESTORE(cmd.Args)
	// Set
	case "SADD":
		res = cmdSADD(cmd.Args)
	case "SREM":
		res = cmdSREM(cmd.Args)
	case "SCARD":
		res = cmdSCARD(cmd.Args)
	case "SMEMBERS":
		res = cmdSMEMBERS(cmd.Args)
	case "SISMEMBER":
		res = cmdSISMEMBER(cmd.Args)
	case "SMISMEMBER":
		res = cmdSMISMEMBER(cmd.Args)
	case "SRAND":
		res = cmdSRAND(cmd.Args)
	case "SPOP":
		res = cmdSPOP(cmd.Args)
	// Sorted set
	case "ZADD":
		res = cmdZADD(cmd.Args)
	case "ZRANK":
		res = cmdZRANK(cmd.Args)
	case "ZREM":
		res = cmdZREM(cmd.Args)
	case "ZSCORE":
		res = cmdZSCORE(cmd.Args)
	case "ZCARD":
		res = cmdZCARD(cmd.Args)
	// Geo Hash
	case "GEOADD":
		res = cmdGEOADD(cmd.Args)
	case "GEODIST":
		res = cmdGEODIST(cmd.Args)
	case "GEOHASH":
		res = cmdGEOHASH(cmd.Args)
	case "GEOSEARCH":
		res = cmdGEOSEARCH(cmd.Args)
	case "GEOPOS":
		res = cmdGEOPOS(cmd.Args)
	// Bloom filter
	case "BF.RESERVE":
		res = cmdBFRESERVE(cmd.Args)
	case "BF.INFO":
		res = cmdBFINFO(cmd.Args)
	case "BF.MADD":
		res = cmdBFMADD(cmd.Args)
	case "BF.EXISTS":
		res = cmdBFEXISTS(cmd.Args)
	case "BF.MEXISTS":
		res = cmdBFMEXISTS(cmd.Args)
	// Count-Min Sketch
	case "CMS.INITBYDIM":
		res = cmdCMSINITBYDIM(cmd.Args)
	case "CMS.INITBYPROB":
		res = cmdCMSINITBYPROB(cmd.Args)
	case "CMS.INCRBY":
		res = cmdCMSINCRBY(cmd.Args)
	case "CMS.QUERY":
		res = cmdCMSQUERY(cmd.Args)
	// Morris counter
	case "MORRIS.INITBYDIM":
		res = cmdMORRISINITBYDIM(cmd.Args)
	case "MORRIS.INITBYPROB":
		res = cmdMORRISINITBYPROB(cmd.Args)
	case "MORRIS.INCRBY":
		res = cmdMORRISINCRBY(cmd.Args)
	case "MORRIS.QUERY":
		res = cmdMORRISQUERY(cmd.Args)
	case "MORRIS.INFO":
		res = cmdMORRISINFO(cmd.Args)
	// HyperLogLog
	case "PFADD":
		res = cmdPFADD(cmd.Args)
	case "PFCOUNT":
		res = cmdPFCOUNT(cmd.Args)
	case "PFMERGE":
		res = cmdPFMERGE(cmd.Args)
	// Cuckoo filter
	case "CF.RESERVE":
		res = cmdCFRESERVE(cmd.Args)
	case "CF.ADD":
		res = cmdCFADD(cmd.Args)
	case "CF.ADDNX":
		res = cmdCFADDNX(cmd.Args)
	case "CF.EXISTS":
		res = cmdCFEXISTS(cmd.Args)
	case "CF.MEXISTS":
		res = cmdCFMEXISTS(cmd.Args)
	case "CF.DEL":
		res = cmdCFDEL(cmd.Args)
	case "CF.COUNT":
		res = cmdCFCOUNT(cmd.Args)
	case "CF.INFO":
		res = cmdCFINFO(cmd.Args)
	default:
		return errors.New(fmt.Sprintf("command not found: %s", cmd.Cmd))
	}

	// Recorded before the reply is written. FlushAOF runs between execution and
	// the write phase, so under appendfsync always the client hears "OK" only
	// once the log holding that OK is on disk.
	aofCommit(cmd, res)

	_, err := c.Write(res)
	return err
}
