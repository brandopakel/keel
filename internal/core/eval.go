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
	case "INCR":
		res = cmdINCR(cmd.Args)
	case "LCS":
		res = cmdLCS(cmd.Args)
	case "DBSIZE":
		res = cmdDBSIZE(cmd.Args)
	case "MEMORY":
		res = cmdMEMORY(cmd.Args)
	case "INFO":
		res = cmdINFO(cmd.Args)
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
	_, err := c.Write(res)
	return err
}
