package core

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/brandopakel/keel/internal/data_structure"
)

// The append-only file.
//
// Everything else in this server is in memory and stays there, so a restart has
// always been a flush. The append-only file is the smallest honest way out of
// that: every command that changes the dataset is written to the end of a file,
// and starting up means replaying it. No index, no pages, no btree - the
// durability comes entirely from the fact that appending is the one file
// operation that is cheap and hard to get wrong.
//
// The log records commands rather than the data they produce, which is what
// makes it cheap to write and expensive to load: a key written a million times
// is a million lines, and replaying is as slow as the original run was. That is
// the trade Redis makes too, and it is why an AOF eventually needs rewriting
// into a shorter log that produces the same state. Incremental rewrites do that.
//
// # What has to be recorded, and what has to be rewritten
//
// A log is only worth having if replaying it produces what was there before, so
// what goes into it is not simply "the commands that arrived".
//
//   - Reads are not recorded. Nothing else would be wrong if they were, but the
//     file would grow with traffic rather than with changes.
//   - A command that fails is not recorded, on the reasoning that a reply
//     beginning with '-' changed nothing. That is a heuristic and it is stated
//     here rather than hidden: a command that half-succeeded and then errored
//     would be missed by it. None of the commands here do that today.
//   - SPOP is recorded as the SREM it turned out to be. It removes members
//     chosen at random, so replaying the command itself would remove different
//     ones and the set would diverge from the first restart onwards.
//   - EXPIRE and SET with a TTL are recorded as PEXPIREAT, which names an
//     instant instead of a duration. Replaying "expire in ten seconds" a day
//     later grants ten fresh seconds, so every restart would renew every TTL in
//     the keyspace.
//   - Expiry and eviction are recorded as DEL, through the OnRemove hook of
//     the engine's space. Neither has a command behind it, and a log
//     that omits them replays into a keyspace holding keys the original had
//     already dropped.
//
// Redis arrives at all five of these rules, by the same route.
//
// Each Engine holds one aofState, as e.aof, for the log it records in.
type aofState struct {
	file        *os.File
	digest      hash.Hash
	digestBytes int64
	// path is the file the descriptor above was opened on.
	//
	// Kept here rather than read from config when needed. A rewrite that took
	// the path from config would write the new log wherever the setting
	// currently points, which is not necessarily the file being appended to -
	// so a caller that opened one path would have its rewrite land in another,
	// leaving the first to grow forever and the second to be overwritten by
	// something that never belonged to it.
	path string
	// buf coalesces ordinary commands until the loop flushes. Large transcripts
	// drain bounded fragments without syncing or advancing a rewrite mid-record.
	buf []byte
	// staged is what the command currently executing wants recorded in its
	// place, for the commands that must not be replayed as they arrived:
	// each record's parts, flat, after a header that counts them - see
	// aof_staging.go.
	staged       []stagedPart
	commandStart int
	// A scope preserves the unpublished replication prefix across bounded
	// buffer drains. Opaque commands publish their final image instead.
	commandActive, commandOpaque, commandChanged bool
	// transaction is set while EXEC runs its commands, and transactionLogged
	// once MULTI has been written ahead of the block's first record. A
	// transaction that records nothing writes no frame; see transaction.go.
	transaction, transactionLogged bool
	// midRecord is set while the file ends in the middle of a record, which
	// a drain of a large record leaves it until the record's last part is
	// written (writeAOFBuffer).
	midRecord bool
	// replaying suppresses recording, so loading a log does not write it back
	// into itself.
	recovered   []string
	replaying   bool
	lastSync    time.Time
	syncPending chan error
	syncOffset  uint64
	dirty       bool
	failed      error
	// skip says the running command is not logged as it arrived: it
	// changed nothing (an NX that declined), or it records itself
	// (recordsItself), its staged records being all the log takes.
	skip bool

	// baseSize is the file's size after the last rewrite, and written what has
	// been appended since. Their ratio is what the automatic rewrite triggers
	// on: what matters is how much of the file is superseded, not how big it
	// is, and only a comparison against the size the data actually needs can
	// tell those apart.
	baseSize    int64
	rewriteBase int64
	written     int64
	rewrites    int
	lastKeys    int
}

// writeCommands are the commands that change the dataset. A command absent from
// here is a read, and a read is never recorded.
//
// Listed explicitly rather than derived, because the cost of the two mistakes
// is not symmetric: forgetting to add a write command loses data silently at
// the next restart, while a read listed by accident only makes the file bigger.
// An explicit list is the one that can be read against eval.go and checked.
var writeCommands = map[string]bool{
	"SET": true, "SETNX": true, "MSET": true, "DEL": true, "UNLINK": true, "FLUSHDB": true,
	"APPEND": true, "SETRANGE": true, "GETDEL": true, "GETEX": true, "GETSET": true, "MSETNX": true,
	"INCRBYFLOAT": true,
	"SETEX":       true, "PSETEX": true,
	"EXPIRE": true, "PEXPIREAT": true, "PEXPIRE": true, "EXPIREAT": true, "PERSIST": true, "INCR": true, "INCRBY": true, "DECR": true, "DECRBY": true,
	"HSET": true, "HSETNX": true, "HDEL": true, "HINCRBY": true,
	"LPUSH": true, "RPUSH": true, "LPOP": true, "RPOP": true, "LTRIM": true, "LSET": true,
	"LPUSHX": true, "RPUSHX": true, "LINSERT": true, "LREM": true, "LMOVE": true, "RPOPLPUSH": true,
	"LMOVEM": true, "LMPOP": true,
	"SADD": true, "SREM": true, "SPOP": true,
	"ZADD": true, "ZREM": true, "ZINCRBY": true, "ZPOPMIN": true, "ZPOPMAX": true,
	"GEOADD":     true,
	"BF.RESERVE": true, "BF.ADD": true, "BF.MADD": true,
	"CMS.INITBYDIM": true, "CMS.INITBYPROB": true, "CMS.INCRBY": true, "CMS.MERGE": true,
	"BF.INSERT": true, "CF.INSERT": true, "CF.INSERTNX": true, "CF.COMPACT": true,
	"MORRIS.INITBYDIM": true, "MORRIS.INITBYPROB": true, "MORRIS.INCRBY": true,
	"PFADD": true, "PFMERGE": true,
	"CF.RESERVE": true, "CF.ADD": true, "CF.ADDNX": true, "CF.DEL": true,
	"KEEL.RESTORE": true, "MEMKV.RESTORE": true,
}

// persistedName is the name a command is recorded under, which is not always
// the name it arrived under.
//
// MEMKV.RESTORE is accepted so that logs written before the rename replay, but
// a command is appended to the log as it was received - so a client sending the
// old name would write the old name into a brand new file, and the log would
// carry the alias forward forever. Read both, write one.
//
// UNLINK is recorded as the DEL it is, so a log written by this build still
// replays on one from before UNLINK existed - which is what a rollback needs.
func persistedName(cmd string) string {
	switch cmd {
	case "MEMKV.RESTORE":
		return "KEEL.RESTORE"
	case "UNLINK":
		return "DEL"
	}
	return cmd
}

// AOFEnabled reports whether the log is on.
func (e *Engine) AOFEnabled() bool { return e.aof.file != nil }

// layoutPadAOF is layout padding for a throwaway control build: never run.
//
//go:noinline
func layoutPadAOF(s *[64]uint64) {
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
	s[20] += s[29] ^ 0x6a99b44c
	s[27] += s[42] ^ 0x8d12dfd
	s[34] += s[55] ^ 0xa708a7ae
	s[41] += s[4] ^ 0x4540215f
	s[48] += s[17] ^ 0xe3779b10
	s[55] += s[30] ^ 0x81af14c1
	s[62] += s[43] ^ 0x1fe68e72
	s[5] += s[56] ^ 0xbe1e0823
	s[12] += s[5] ^ 0x5c5581d4
	s[19] += s[18] ^ 0xfa8cfb85
	s[26] += s[31] ^ 0x98c47536
	s[33] += s[44] ^ 0x36fbeee7
	s[40] += s[57] ^ 0xd5336898
	s[47] += s[6] ^ 0x736ae249
	s[54] += s[19] ^ 0x11a25bfa
	s[61] += s[32] ^ 0xafd9d5ab
	s[4] += s[45] ^ 0x4e114f5c
	s[11] += s[58] ^ 0xec48c90d
	s[18] += s[7] ^ 0x8a8042be
	s[25] += s[20] ^ 0x28b7bc6f
	s[32] += s[33] ^ 0xc6ef3620
	s[39] += s[46] ^ 0x6526afd1
	s[46] += s[59] ^ 0x35e2982
	s[53] += s[8] ^ 0xa195a333
	s[60] += s[21] ^ 0x3fcd1ce4
	s[3] += s[34] ^ 0xde049695
	s[10] += s[47] ^ 0x7c3c1046
	s[17] += s[60] ^ 0x1a7389f7
	s[24] += s[9] ^ 0xb8ab03a8
	s[31] += s[22] ^ 0x56e27d59
	s[38] += s[35] ^ 0xf519f70a
	s[45] += s[48] ^ 0x935170bb
	s[52] += s[61] ^ 0x3188ea6c
	s[59] += s[10] ^ 0xcfc0641d
	s[2] += s[23] ^ 0x6df7ddce
	s[9] += s[36] ^ 0xc2f577f
	s[16] += s[49] ^ 0xaa66d130
	s[23] += s[62] ^ 0x489e4ae1
	s[30] += s[11] ^ 0xe6d5c492
	s[37] += s[24] ^ 0x850d3e43
	s[44] += s[37] ^ 0x2344b7f4
	s[51] += s[50] ^ 0xc17c31a5
	s[58] += s[63] ^ 0x5fb3ab56
	s[1] += s[12] ^ 0xfdeb2507
	s[8] += s[25] ^ 0x9c229eb8
	s[15] += s[38] ^ 0x3a5a1869
	s[22] += s[51] ^ 0xd891921a
	s[29] += s[0] ^ 0x76c90bcb
	s[36] += s[13] ^ 0x1500857c
	s[43] += s[26] ^ 0xb337ff2d
	s[50] += s[39] ^ 0x516f78de
	s[57] += s[52] ^ 0xefa6f28f
	s[0] += s[1] ^ 0x8dde6c40
	s[7] += s[14] ^ 0x2c15e5f1
	s[14] += s[27] ^ 0xca4d5fa2
	s[21] += s[40] ^ 0x6884d953
	s[28] += s[53] ^ 0x6bc5304
	s[35] += s[2] ^ 0xa4f3ccb5
	s[42] += s[15] ^ 0x432b4666
	s[49] += s[28] ^ 0xe162c017
	s[56] += s[41] ^ 0x7f9a39c8
	s[63] += s[54] ^ 0x1dd1b379
	s[6] += s[3] ^ 0xbc092d2a
	s[13] += s[16] ^ 0x5a40a6db
	s[20] += s[29] ^ 0xf878208c
	s[27] += s[42] ^ 0x96af9a3d
	s[34] += s[55] ^ 0x34e713ee
	s[41] += s[4] ^ 0xd31e8d9f
	s[48] += s[17] ^ 0x71560750
	s[55] += s[30] ^ 0xf8d8101
	s[62] += s[43] ^ 0xadc4fab2
	s[5] += s[56] ^ 0x4bfc7463
	s[12] += s[5] ^ 0xea33ee14
	s[19] += s[18] ^ 0x886b67c5
	s[26] += s[31] ^ 0x26a2e176
	s[33] += s[44] ^ 0xc4da5b27
	s[40] += s[57] ^ 0x6311d4d8
	s[47] += s[6] ^ 0x1494e89
	s[54] += s[19] ^ 0x9f80c83a
	s[61] += s[32] ^ 0x3db841eb
	s[4] += s[45] ^ 0xdbefbb9c
	s[11] += s[58] ^ 0x7a27354d
	s[18] += s[7] ^ 0x185eaefe
	s[25] += s[20] ^ 0xb69628af
	s[32] += s[33] ^ 0x54cda260
	s[39] += s[46] ^ 0xf3051c11
	s[46] += s[59] ^ 0x913c95c2
	s[53] += s[8] ^ 0x2f740f73
	s[60] += s[21] ^ 0xcdab8924
	s[3] += s[34] ^ 0x6be302d5
	s[10] += s[47] ^ 0xa1a7c86
	s[17] += s[60] ^ 0xa851f637
	s[24] += s[9] ^ 0x46896fe8
	s[31] += s[22] ^ 0xe4c0e999
	s[38] += s[35] ^ 0x82f8634a
	s[45] += s[48] ^ 0x212fdcfb
	s[52] += s[61] ^ 0xbf6756ac
	s[59] += s[10] ^ 0x5d9ed05d
	s[2] += s[23] ^ 0xfbd64a0e
	s[9] += s[36] ^ 0x9a0dc3bf
	s[16] += s[49] ^ 0x38453d70
	s[23] += s[62] ^ 0xd67cb721
	s[30] += s[11] ^ 0x74b430d2
	s[37] += s[24] ^ 0x12ebaa83
	s[44] += s[37] ^ 0xb1232434
	s[51] += s[50] ^ 0x4f5a9de5
	s[58] += s[63] ^ 0xed921796
	s[1] += s[12] ^ 0x8bc99147
	s[8] += s[25] ^ 0x2a010af8
	s[15] += s[38] ^ 0xc83884a9
	s[22] += s[51] ^ 0x666ffe5a
	s[29] += s[0] ^ 0x4a7780b
	s[36] += s[13] ^ 0xa2def1bc
	s[43] += s[26] ^ 0x41166b6d
	s[50] += s[39] ^ 0xdf4de51e
	s[57] += s[52] ^ 0x7d855ecf
	s[0] += s[1] ^ 0x1bbcd880
	s[7] += s[14] ^ 0xb9f45231
	s[14] += s[27] ^ 0x582bcbe2
	s[21] += s[40] ^ 0xf6634593
	s[28] += s[53] ^ 0x949abf44
	s[35] += s[2] ^ 0x32d238f5
	s[42] += s[15] ^ 0xd109b2a6
	s[49] += s[28] ^ 0x6f412c57
	s[56] += s[41] ^ 0xd78a608
	s[63] += s[54] ^ 0xabb01fb9
	s[6] += s[3] ^ 0x49e7996a
	s[13] += s[16] ^ 0xe81f131b
	s[20] += s[29] ^ 0x86568ccc
	s[27] += s[42] ^ 0x248e067d
	s[34] += s[55] ^ 0xc2c5802e
	s[41] += s[4] ^ 0x60fcf9df
	s[48] += s[17] ^ 0xff347390
	s[55] += s[30] ^ 0x9d6bed41
	s[62] += s[43] ^ 0x3ba366f2
	s[5] += s[56] ^ 0xd9dae0a3
	s[12] += s[5] ^ 0x78125a54
	s[19] += s[18] ^ 0x1649d405
	s[26] += s[31] ^ 0xb4814db6
	s[33] += s[44] ^ 0x52b8c767
	s[40] += s[57] ^ 0xf0f04118
	s[47] += s[6] ^ 0x8f27bac9
	s[54] += s[19] ^ 0x2d5f347a
	s[61] += s[32] ^ 0xcb96ae2b
	s[4] += s[45] ^ 0x69ce27dc
	s[11] += s[58] ^ 0x805a18d
	s[18] += s[7] ^ 0xa63d1b3e
	s[25] += s[20] ^ 0x447494ef
	s[32] += s[33] ^ 0xe2ac0ea0
	s[39] += s[46] ^ 0x80e38851
	s[46] += s[59] ^ 0x1f1b0202
	s[53] += s[8] ^ 0xbd527bb3
	s[60] += s[21] ^ 0x5b89f564
	s[3] += s[34] ^ 0xf9c16f15
	s[10] += s[47] ^ 0x97f8e8c6
	s[17] += s[60] ^ 0x36306277
	s[24] += s[9] ^ 0xd467dc28
	s[31] += s[22] ^ 0x729f55d9
	s[38] += s[35] ^ 0x10d6cf8a
	s[45] += s[48] ^ 0xaf0e493b
	s[52] += s[61] ^ 0x4d45c2ec
	s[59] += s[10] ^ 0xeb7d3c9d
	s[2] += s[23] ^ 0x89b4b64e
	s[9] += s[36] ^ 0x27ec2fff
	s[16] += s[49] ^ 0xc623a9b0
	s[23] += s[62] ^ 0x645b2361
	s[30] += s[11] ^ 0x2929d12
	s[37] += s[24] ^ 0xa0ca16c3
	s[44] += s[37] ^ 0x3f019074
	s[51] += s[50] ^ 0xdd390a25
	s[58] += s[63] ^ 0x7b7083d6
	s[1] += s[12] ^ 0x19a7fd87
	s[8] += s[25] ^ 0xb7df7738
	s[15] += s[38] ^ 0x5616f0e9
	s[22] += s[51] ^ 0xf44e6a9a
	s[29] += s[0] ^ 0x9285e44b
	s[36] += s[13] ^ 0x30bd5dfc
	s[43] += s[26] ^ 0xcef4d7ad
	s[50] += s[39] ^ 0x6d2c515e
	s[57] += s[52] ^ 0xb63cb0f
	s[0] += s[1] ^ 0xa99b44c0
	s[7] += s[14] ^ 0x47d2be71
	s[14] += s[27] ^ 0xe60a3822
	s[21] += s[40] ^ 0x8441b1d3
	s[28] += s[53] ^ 0x22792b84
	s[35] += s[2] ^ 0xc0b0a535
	s[42] += s[15] ^ 0x5ee81ee6
	s[49] += s[28] ^ 0xfd1f9897
	s[56] += s[41] ^ 0x9b571248
	s[63] += s[54] ^ 0x398e8bf9
	s[6] += s[3] ^ 0xd7c605aa
	s[13] += s[16] ^ 0x75fd7f5b
	s[20] += s[29] ^ 0x1434f90c
	s[27] += s[42] ^ 0xb26c72bd
	s[34] += s[55] ^ 0x50a3ec6e
	s[41] += s[4] ^ 0xeedb661f
	s[48] += s[17] ^ 0x8d12dfd0
	s[55] += s[30] ^ 0x2b4a5981
	s[62] += s[43] ^ 0xc981d332
	s[5] += s[56] ^ 0x67b94ce3
	s[12] += s[5] ^ 0x5f0c694
	s[19] += s[18] ^ 0xa4284045
	s[26] += s[31] ^ 0x425fb9f6
	s[33] += s[44] ^ 0xe09733a7
	s[40] += s[57] ^ 0x7ecead58
	s[47] += s[6] ^ 0x1d062709
	s[54] += s[19] ^ 0xbb3da0ba
	s[61] += s[32] ^ 0x59751a6b
	s[4] += s[45] ^ 0xf7ac941c
	s[11] += s[58] ^ 0x95e40dcd
	s[18] += s[7] ^ 0x341b877e
	s[25] += s[20] ^ 0xd253012f
	s[32] += s[33] ^ 0x708a7ae0
	s[39] += s[46] ^ 0xec1f491
	s[46] += s[59] ^ 0xacf96e42
	s[53] += s[8] ^ 0x4b30e7f3
	s[60] += s[21] ^ 0xe96861a4
	s[3] += s[34] ^ 0x879fdb55
	s[10] += s[47] ^ 0x25d75506
	s[17] += s[60] ^ 0xc40eceb7
	s[24] += s[9] ^ 0x62464868
	s[31] += s[22] ^ 0x7dc219
	s[38] += s[35] ^ 0x9eb53bca
	s[45] += s[48] ^ 0x3cecb57b
	s[52] += s[61] ^ 0xdb242f2c
	s[59] += s[10] ^ 0x795ba8dd
	s[2] += s[23] ^ 0x1793228e
	s[9] += s[36] ^ 0xb5ca9c3f
	s[16] += s[49] ^ 0x540215f0
	s[23] += s[62] ^ 0xf2398fa1
	s[30] += s[11] ^ 0x90710952
	s[37] += s[24] ^ 0x2ea88303
	s[44] += s[37] ^ 0xccdffcb4
	s[51] += s[50] ^ 0x6b177665
	s[58] += s[63] ^ 0x94ef016
	s[1] += s[12] ^ 0xa78669c7
	s[8] += s[25] ^ 0x45bde378
	s[15] += s[38] ^ 0xe3f55d29
	s[22] += s[51] ^ 0x822cd6da
	s[29] += s[0] ^ 0x2064508b
	s[36] += s[13] ^ 0xbe9bca3c
	s[43] += s[26] ^ 0x5cd343ed
	s[50] += s[39] ^ 0xfb0abd9e
	s[57] += s[52] ^ 0x9942374f
	s[0] += s[1] ^ 0x3779b100
	s[7] += s[14] ^ 0xd5b12ab1
	s[14] += s[27] ^ 0x73e8a462
	s[21] += s[40] ^ 0x12201e13
	s[28] += s[53] ^ 0xb05797c4
	s[35] += s[2] ^ 0x4e8f1175
	s[42] += s[15] ^ 0xecc68b26
	s[49] += s[28] ^ 0x8afe04d7
	s[56] += s[41] ^ 0x29357e88
	s[63] += s[54] ^ 0xc76cf839
	s[6] += s[3] ^ 0x65a471ea
	s[13] += s[16] ^ 0x3dbeb9b
	s[20] += s[29] ^ 0xa213654c
	s[27] += s[42] ^ 0x404adefd
	s[34] += s[55] ^ 0xde8258ae
	s[41] += s[4] ^ 0x7cb9d25f
	s[48] += s[17] ^ 0x1af14c10
	s[55] += s[30] ^ 0xb928c5c1
	s[62] += s[43] ^ 0x57603f72
	s[5] += s[56] ^ 0xf597b923
	s[12] += s[5] ^ 0x93cf32d4
	s[19] += s[18] ^ 0x3206ac85
	s[26] += s[31] ^ 0xd03e2636
	s[33] += s[44] ^ 0x6e759fe7
	s[40] += s[57] ^ 0xcad1998
	s[47] += s[6] ^ 0xaae49349
	s[54] += s[19] ^ 0x491c0cfa
	s[61] += s[32] ^ 0xe75386ab
	s[4] += s[45] ^ 0x858b005c
	s[11] += s[58] ^ 0x23c27a0d
	s[18] += s[7] ^ 0xc1f9f3be
	s[25] += s[20] ^ 0x60316d6f
	s[32] += s[33] ^ 0xfe68e720
	s[39] += s[46] ^ 0x9ca060d1
	s[46] += s[59] ^ 0x3ad7da82
	s[53] += s[8] ^ 0xd90f5433
	s[60] += s[21] ^ 0x7746cde4
	s[3] += s[34] ^ 0x157e4795
	s[10] += s[47] ^ 0xb3b5c146
	s[17] += s[60] ^ 0x51ed3af7
	s[24] += s[9] ^ 0xf024b4a8
	s[31] += s[22] ^ 0x8e5c2e59
	s[38] += s[35] ^ 0x2c93a80a
	s[45] += s[48] ^ 0xcacb21bb
	s[52] += s[61] ^ 0x69029b6c
	s[59] += s[10] ^ 0x73a151d
	s[2] += s[23] ^ 0xa5718ece
	s[9] += s[36] ^ 0x43a9087f
	s[16] += s[49] ^ 0xe1e08230
	s[23] += s[62] ^ 0x8017fbe1
	s[30] += s[11] ^ 0x1e4f7592
	s[37] += s[24] ^ 0xbc86ef43
	s[44] += s[37] ^ 0x5abe68f4
	s[51] += s[50] ^ 0xf8f5e2a5
	s[58] += s[63] ^ 0x972d5c56
	s[1] += s[12] ^ 0x3564d607
	s[8] += s[25] ^ 0xd39c4fb8
	s[15] += s[38] ^ 0x71d3c969
	s[22] += s[51] ^ 0x100b431a
	s[29] += s[0] ^ 0xae42bccb
	s[36] += s[13] ^ 0x4c7a367c
	s[43] += s[26] ^ 0xeab1b02d
	s[50] += s[39] ^ 0x88e929de
	s[57] += s[52] ^ 0x2720a38f
	s[0] += s[1] ^ 0xc5581d40
	s[7] += s[14] ^ 0x638f96f1
	s[14] += s[27] ^ 0x1c710a2
	s[21] += s[40] ^ 0x9ffe8a53
	s[28] += s[53] ^ 0x3e360404
	s[35] += s[2] ^ 0xdc6d7db5
	s[42] += s[15] ^ 0x7aa4f766
	s[49] += s[28] ^ 0x18dc7117
	s[56] += s[41] ^ 0xb713eac8
	s[63] += s[54] ^ 0x554b6479
	s[6] += s[3] ^ 0xf382de2a
	s[13] += s[16] ^ 0x91ba57db
	s[20] += s[29] ^ 0x2ff1d18c
	s[27] += s[42] ^ 0xce294b3d
	s[34] += s[55] ^ 0x6c60c4ee
	s[41] += s[4] ^ 0xa983e9f
	s[48] += s[17] ^ 0xa8cfb850
	s[55] += s[30] ^ 0x47073201
	s[62] += s[43] ^ 0xe53eabb2
	s[5] += s[56] ^ 0x83762563
	s[12] += s[5] ^ 0x21ad9f14
	s[19] += s[18] ^ 0xbfe518c5
	s[26] += s[31] ^ 0x5e1c9276
	s[33] += s[44] ^ 0xfc540c27
	s[40] += s[57] ^ 0x9a8b85d8
	s[47] += s[6] ^ 0x38c2ff89
	s[54] += s[19] ^ 0xd6fa793a
	s[61] += s[32] ^ 0x7531f2eb
	s[4] += s[45] ^ 0x13696c9c
	s[11] += s[58] ^ 0xb1a0e64d
	s[18] += s[7] ^ 0x4fd85ffe
	s[25] += s[20] ^ 0xee0fd9af
	s[32] += s[33] ^ 0x8c475360
	s[39] += s[46] ^ 0x2a7ecd11
	s[46] += s[59] ^ 0xc8b646c2
	s[53] += s[8] ^ 0x66edc073
	s[60] += s[21] ^ 0x5253a24
	s[3] += s[34] ^ 0xa35cb3d5
	s[10] += s[47] ^ 0x41942d86
	s[17] += s[60] ^ 0xdfcba737
	s[24] += s[9] ^ 0x7e0320e8
	s[31] += s[22] ^ 0x1c3a9a99
	s[38] += s[35] ^ 0xba72144a
	s[45] += s[48] ^ 0x58a98dfb
	s[52] += s[61] ^ 0xf6e107ac
	s[59] += s[10] ^ 0x9518815d
	s[2] += s[23] ^ 0x334ffb0e
	s[9] += s[36] ^ 0xd18774bf
	s[16] += s[49] ^ 0x6fbeee70
	s[23] += s[62] ^ 0xdf66821
	s[30] += s[11] ^ 0xac2de1d2
	s[37] += s[24] ^ 0x4a655b83
	s[44] += s[37] ^ 0xe89cd534
	s[51] += s[50] ^ 0x86d44ee5
	s[58] += s[63] ^ 0x250bc896
	s[1] += s[12] ^ 0xc3434247
	s[8] += s[25] ^ 0x617abbf8
	s[15] += s[38] ^ 0xffb235a9
	s[22] += s[51] ^ 0x9de9af5a
	s[29] += s[0] ^ 0x3c21290b
	s[36] += s[13] ^ 0xda58a2bc
	s[43] += s[26] ^ 0x78901c6d
	s[50] += s[39] ^ 0x16c7961e
	s[57] += s[52] ^ 0xb4ff0fcf
	s[0] += s[1] ^ 0x53368980
	s[7] += s[14] ^ 0xf16e0331
	s[14] += s[27] ^ 0x8fa57ce2
	s[21] += s[40] ^ 0x2ddcf693
	s[28] += s[53] ^ 0xcc147044
	s[35] += s[2] ^ 0x6a4be9f5
	s[42] += s[15] ^ 0x88363a6
	s[49] += s[28] ^ 0xa6badd57
	s[56] += s[41] ^ 0x44f25708
	s[63] += s[54] ^ 0xe329d0b9
	s[6] += s[3] ^ 0x81614a6a
	s[13] += s[16] ^ 0x1f98c41b
	s[20] += s[29] ^ 0xbdd03dcc
	s[27] += s[42] ^ 0x5c07b77d
	s[34] += s[55] ^ 0xfa3f312e
	s[41] += s[4] ^ 0x9876aadf
	s[48] += s[17] ^ 0x36ae2490
	s[55] += s[30] ^ 0xd4e59e41
	s[62] += s[43] ^ 0x731d17f2
	s[5] += s[56] ^ 0x115491a3
	s[12] += s[5] ^ 0xaf8c0b54
	s[19] += s[18] ^ 0x4dc38505
	s[26] += s[31] ^ 0xebfafeb6
	s[33] += s[44] ^ 0x8a327867
	s[40] += s[57] ^ 0x2869f218
	s[47] += s[6] ^ 0xc6a16bc9
	s[54] += s[19] ^ 0x64d8e57a
	s[61] += s[32] ^ 0x3105f2b
	s[4] += s[45] ^ 0xa147d8dc
	s[11] += s[58] ^ 0x3f7f528d
	s[18] += s[7] ^ 0xddb6cc3e
	s[25] += s[20] ^ 0x7bee45ef
	s[32] += s[33] ^ 0x1a25bfa0
	s[39] += s[46] ^ 0xb85d3951
	s[46] += s[59] ^ 0x5694b302
	s[53] += s[8] ^ 0xf4cc2cb3
	s[60] += s[21] ^ 0x9303a664
	s[3] += s[34] ^ 0x313b2015
	s[10] += s[47] ^ 0xcf7299c6
	s[17] += s[60] ^ 0x6daa1377
	s[24] += s[9] ^ 0xbe18d28
	s[31] += s[22] ^ 0xaa1906d9
	s[38] += s[35] ^ 0x4850808a
	s[45] += s[48] ^ 0xe687fa3b
	s[52] += s[61] ^ 0x84bf73ec
	s[59] += s[10] ^ 0x22f6ed9d
	s[2] += s[23] ^ 0xc12e674e
	s[9] += s[36] ^ 0x5f65e0ff
	s[16] += s[49] ^ 0xfd9d5ab0
	s[23] += s[62] ^ 0x9bd4d461
	s[30] += s[11] ^ 0x3a0c4e12
	s[37] += s[24] ^ 0xd843c7c3
	s[44] += s[37] ^ 0x767b4174
	s[51] += s[50] ^ 0x14b2bb25
	s[58] += s[63] ^ 0xb2ea34d6
	s[1] += s[12] ^ 0x5121ae87
	s[8] += s[25] ^ 0xef592838
	s[15] += s[38] ^ 0x8d90a1e9
	s[22] += s[51] ^ 0x2bc81b9a
	s[29] += s[0] ^ 0xc9ff954b
	s[36] += s[13] ^ 0x68370efc
	s[43] += s[26] ^ 0x66e88ad
	s[50] += s[39] ^ 0xa4a6025e
	s[57] += s[52] ^ 0x42dd7c0f
	s[0] += s[1] ^ 0xe114f5c0
	s[7] += s[14] ^ 0x7f4c6f71
	s[14] += s[27] ^ 0x1d83e922
	s[21] += s[40] ^ 0xbbbb62d3
	s[28] += s[53] ^ 0x59f2dc84
	s[35] += s[2] ^ 0xf82a5635
	s[42] += s[15] ^ 0x9661cfe6
	s[49] += s[28] ^ 0x34994997
	s[56] += s[41] ^ 0xd2d0c348
	s[63] += s[54] ^ 0x71083cf9
	s[6] += s[3] ^ 0xf3fb6aa
	s[13] += s[16] ^ 0xad77305b
	s[20] += s[29] ^ 0x4baeaa0c
	s[27] += s[42] ^ 0xe9e623bd
	s[34] += s[55] ^ 0x881d9d6e
	s[41] += s[4] ^ 0x2655171f
	s[48] += s[17] ^ 0xc48c90d0
	s[55] += s[30] ^ 0x62c40a81
	s[62] += s[43] ^ 0xfb8432
	s[5] += s[56] ^ 0x9f32fde3
	s[12] += s[5] ^ 0x3d6a7794
	s[19] += s[18] ^ 0xdba1f145
	s[26] += s[31] ^ 0x79d96af6
	s[33] += s[44] ^ 0x1810e4a7
	s[40] += s[57] ^ 0xb6485e58
	s[47] += s[6] ^ 0x547fd809
	s[54] += s[19] ^ 0xf2b751ba
	s[61] += s[32] ^ 0x90eecb6b
	s[4] += s[45] ^ 0x2f26451c
	s[11] += s[58] ^ 0xcd5dbecd
	s[18] += s[7] ^ 0x6b95387e
	s[25] += s[20] ^ 0x9ccb22f
	s[32] += s[33] ^ 0xa8042be0
	s[39] += s[46] ^ 0x463ba591
	s[46] += s[59] ^ 0xe4731f42
	s[53] += s[8] ^ 0x82aa98f3
	s[60] += s[21] ^ 0x20e212a4
	s[3] += s[34] ^ 0xbf198c55
	s[10] += s[47] ^ 0x5d510606
	s[17] += s[60] ^ 0xfb887fb7
	s[24] += s[9] ^ 0x99bff968
	s[31] += s[22] ^ 0x37f77319
	s[38] += s[35] ^ 0xd62eecca
	s[45] += s[48] ^ 0x7466667b
	s[52] += s[61] ^ 0x129de02c
	s[59] += s[10] ^ 0xb0d559dd
	s[2] += s[23] ^ 0x4f0cd38e
	s[9] += s[36] ^ 0xed444d3f
	s[16] += s[49] ^ 0x8b7bc6f0
	s[23] += s[62] ^ 0x29b340a1
	s[30] += s[11] ^ 0xc7eaba52
	s[37] += s[24] ^ 0x66223403
	s[44] += s[37] ^ 0x459adb4
	s[51] += s[50] ^ 0xa2912765
	s[58] += s[63] ^ 0x40c8a116
	s[1] += s[12] ^ 0xdf001ac7
	s[8] += s[25] ^ 0x7d379478
	s[15] += s[38] ^ 0x1b6f0e29
	s[22] += s[51] ^ 0xb9a687da
	s[29] += s[0] ^ 0x57de018b
	s[36] += s[13] ^ 0xf6157b3c
	s[43] += s[26] ^ 0x944cf4ed
	s[50] += s[39] ^ 0x32846e9e
	s[57] += s[52] ^ 0xd0bbe84f
	s[0] += s[1] ^ 0x6ef36200
	s[7] += s[14] ^ 0xd2adbb1
	s[14] += s[27] ^ 0xab625562
	s[21] += s[40] ^ 0x4999cf13
	s[28] += s[53] ^ 0xe7d148c4
	s[35] += s[2] ^ 0x8608c275
	s[42] += s[15] ^ 0x24403c26
	s[49] += s[28] ^ 0xc277b5d7
	s[56] += s[41] ^ 0x60af2f88
	s[63] += s[54] ^ 0xfee6a939
	s[6] += s[3] ^ 0x9d1e22ea
	s[13] += s[16] ^ 0x3b559c9b
	s[20] += s[29] ^ 0xd98d164c
	s[27] += s[42] ^ 0x77c48ffd
	s[34] += s[55] ^ 0x15fc09ae
	s[41] += s[4] ^ 0xb433835f
	s[48] += s[17] ^ 0x526afd10
	s[55] += s[30] ^ 0xf0a276c1
	s[62] += s[43] ^ 0x8ed9f072
	s[5] += s[56] ^ 0x2d116a23
	s[12] += s[5] ^ 0xcb48e3d4
	s[19] += s[18] ^ 0x69805d85
	s[26] += s[31] ^ 0x7b7d736
	s[33] += s[44] ^ 0xa5ef50e7
}

// OpenAOF opens the log for appending and installs the removal hook. Call after
// LoadAOF, so replaying does not append what it is reading.
//
// The log is e's, and the removal hook is installed on e's space, so only e's
// expiry and eviction are recorded in it.
func (e *Engine) OpenAOF(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		f.Close()
		return err
	}

	e.aof.file = f
	e.aof.path = path
	e.aof.lastSync = time.Now()
	e.aof.dirty = false
	e.aof.failed = nil
	e.aof.midRecord = false
	// Counters describe this open file, not whatever the last one did, so they
	// start again with it. Carrying them over would make a fresh log report
	// rewrites it has never had.
	e.aof.rewrites = 0
	e.rewriteBudgetAborts = 0
	e.nextAutoRewrite = time.Time{}
	e.resetRewriteOutcome()
	e.aof.lastKeys = 0
	// Whatever is already on disk is the base the growth trigger measures
	// against, so a server restarted onto an existing log does not immediately
	// decide the log has grown infinitely.
	if info, err := f.Stat(); err == nil {
		e.aof.baseSize = info.Size()
	}
	if err := e.openAOFDigest(path); err != nil {
		f.Close()
		e.aof.file = nil
		return err
	}
	e.aof.rewriteBase = e.aof.baseSize
	// Frequent restarts must not reset compaction's growth target to the
	// ever-growing log. Use a conservative live-state estimate until the next
	// actual rewrite provides an exact baseline. HLL's wire registers remain
	// dense even when its in-memory representation is compact.
	live := e.space.TotalMemUsed()
	const maxInt64 = uint64(1<<63 - 1)
	hllWire := uint64(e.hllStore.Len()) * (16 << 10)
	if live <= (maxInt64-hllWire)/3 {
		if estimate := int64(live*3 + hllWire); estimate < e.aof.rewriteBase {
			e.aof.rewriteBase = estimate
		}
	}
	e.aof.written = 0
	e.appendStarted, e.appendCompleted = 0, 0
	e.appendWritten, e.appendSynced = 0, 0
	for _, key := range e.aof.recovered {
		e.appendAOFCommand("DEL", key)
	}
	e.aof.recovered = nil
	e.space.OnRemove = func(keyspace, key string) {
		if e.aof.file == nil || e.aof.replaying {
			return
		}
		outsideCommand := !e.aof.commandActive
		if outsideCommand {
			e.aofBegin("")
			defer e.aofEnd()
		}
		e.appendAOFCommand("DEL", key)
		// Eviction and expiry remove keys no command named, so a rewrite has to
		// hear about them here or it would carry a key forward that the server
		// had already dropped.
		e.noteRewriteDirty(key)
		e.noteReplicationDirty(key)
	}
	return nil
}

// RenameAOF gives e's open log the name path, in the same directory, and
// syncs the directory, so that the new name survives a crash. A log written
// under a temporary name is published this way only once it holds what it
// must: the server's migration of a legacy log writes the replayed keyspace
// into a log under a temporary name and then renames it onto the name it is
// kept under, so that a crash before then leaves no log under that name, and
// the next start replays the legacy log again.
//
// The descriptor stays open across the rename, and everything e writes after
// it, a rewrite included, goes to the log at its new name.
func (e *Engine) RenameAOF(path string) error {
	if e.aof.file == nil {
		return fmt.Errorf("appendonly is off")
	}
	if e.rewrite.active {
		return fmt.Errorf("a rewrite of %s is in progress", e.aof.path)
	}
	if err := os.Rename(e.aof.path, path); err != nil {
		return err
	}
	e.aof.path = path
	if err := syncDir(filepath.Dir(path)); err != nil {
		// As after a rewrite's rename: the next sync of the log tries the
		// directory again before it counts the log as durable.
		e.unsyncedLogDir = filepath.Dir(path)
		return err
	}
	e.unsyncedLogDir = "" // this sync covers an earlier rename's entry too
	return nil
}

// CloseAOF flushes what is buffered and closes the file. A stop that skipped
// this would lose up to a cycle's worth of acknowledged writes, which is the
// one kind of loss a client has no way to detect.
func (e *Engine) CloseAOF() error {
	e.closeReplicationSnapshot()
	e.CancelRewrite()
	_, _ = e.pollRewriteIO(true)
	if e.aof.file == nil {
		return nil
	}
	// Join both workers even after a failure before closing their descriptor.
	e.pollAppend(true)
	e.pollAOFSync(true)
	err := e.flushAOF(true)
	if cerr := e.aof.file.Close(); err == nil {
		err = cerr
	}
	e.aof.file = nil
	e.aof.path = ""
	e.unsyncedLogDir = ""
	e.aof.buf, e.aof.staged = nil, nil
	e.space.OnRemove = nil
	return err
}

// aofLog reports something about the log that a client did not ask for.
func aofLog(format string, args ...interface{}) {
	log.Printf("appendonly: "+format, args...)
}

// aofBegin resets the staging areas before a command runs.
func (e *Engine) aofBegin(name string) {
	e.aof.commandActive, e.aof.commandChanged = true, false
	e.aof.commandOpaque = isOpaqueReplicationCommand(name)
	e.aof.commandStart = len(e.aof.buf)
	e.aof.skip = false
	e.clearStaged()
}

// aofCommit records what the command that just ran actually did.
func (e *Engine) aofCommit(cmd *Command, reply []byte) {
	if e.aof.file == nil || e.aof.replaying {
		return
	}

	// A key written while a rewrite is walking may already have been recorded
	// at an older value, or not yet reached. Either way the rewrite will write
	// it again at the end from whatever it holds then, so it only has to know
	// which keys those are. Recorded whether or not the log itself takes the
	// command, because a rewrite is a separate question from durability: a read
	// that reaps an expired key changes the keyspace without being logged.
	if e.rewrite.active {
		for _, key := range writtenKeys(cmd) {
			e.noteRewriteDirty(key)
		}
	}

	if writeCommands[cmd.Cmd] {
		for _, key := range writtenKeys(cmd) {
			e.noteReplicationDirty(key)
		}
	}

	switch {
	case len(e.aof.staged) > 0:
		// Staged because the command as it arrived would not replay to the
		// same state, or because the command records itself, so the staged
		// records are what goes in the log.
		e.appendStaged()
	case e.aof.skip:
		// The command changed nothing, or it records itself and staged
		// nothing because it changed nothing: either way it is not logged as
		// it arrived.
	case !writeCommands[cmd.Cmd]:
		// A read. Nothing of the command itself is recorded, but it may still
		// have reaped an expired key, already recorded by the removal hook.
	case len(reply) > 0 && reply[0] == '-':
		// Failed, so by the heuristic in the file comment it changed nothing.
	default:
		e.appendAOFCommand(persistedName(cmd.Cmd), cmd.Args...)
	}

	e.clearStaged()
}

// aofEnd publishes the complete command after its eviction decisions. The
// event loop cannot serve a replica pull in the middle of this serial scope.
func (e *Engine) aofEnd() {
	// A failed drain reslices the retained buffer. Its old commandStart no
	// longer names this slice, and none of that failed suffix may be published.
	if e.aof.file != nil && !e.aof.replaying && e.aof.failed == nil {
		e.recordReplicationV2Commit()
	}
	e.aof.commandActive = false
}

// appendCommand writes one command in the same RESP a client would have sent,
// so that loading the log is exactly the parser that already exists rather than
// a second format to keep in step with the first.
func appendCommand(dst []byte, parts ...string) []byte {
	dst = appendArrayHeader(dst, len(parts))
	for _, p := range parts {
		dst = appendBulkString(dst, p)
	}
	return dst
}

// FlushAOF writes the cycle's commands and syncs according to the policy.
//
// The event loop calls this after executing and before replying, which is the
// ordering that makes appendfsync always mean what it says: a client is told
// its write succeeded only once the write is on disk. Doing it after the
// replies would be faster and would be lying.
func (e *Engine) FlushAOF() error {
	if err := e.flushAOF(false); err != nil {
		return err
	}

	// A rewrite in progress gets one slice per cycle, which is what keeps it
	// from being a stall. This is the right place for it because it is already
	// the once-a-cycle hook: doing it per command would slice a pipelined batch
	// in the middle for no reason.
	if e.rewrite.active {
		return e.AdvanceRewrite()
	}
	e.maybeRewrite()
	return nil
}

// Only the engine's command thread - the event loop, for the server's engine -
// touches its log state. The worker owns one Sync call and publishes its
// result through a buffered channel; there is never a work queue.
func (e *Engine) pollAOFSync(wait bool) {
	if e.aof.syncPending == nil {
		return
	}
	var err error
	if wait {
		err = <-e.aof.syncPending
	} else {
		select {
		case err = <-e.aof.syncPending:
		default:
			return
		}
	}
	e.aof.syncPending = nil
	if err == nil {
		e.appendSynced = max(e.appendSynced, e.aof.syncOffset)
		if e.logFailure.sync != nil {
			e.syncSolved()
		}
		return
	}
	if e.retriesLogFailures() {
		// Retried, as the bytes it failed to sync are still unsynced: the
		// next due sync tries them again (log_failures.go).
		e.syncFailed(err)
		e.aof.dirty = true
		return
	}
	if e.aof.failed == nil {
		e.aof.failed = err
	}
}

func (e *Engine) flushAOF(closing bool) error {
	fsync := e.settings.fsync
	// Under always an engine Open makes syncs outside its lock, as under
	// everysec, and publishes only what a sync has covered: the calls that
	// buffer records while one sync runs wait for the next, which covers them
	// all (group commit). The server's loop, which holds the lock throughout
	// and answers after the flush, syncs in line, as Redis's main thread does.
	syncedOnly := fsync == FsyncAlways && e.driver != nil
	e.pollAOFSync(closing || fsync == FsyncAlways && !syncedOnly)
	if e.aof.file == nil {
		return nil
	}
	if e.aof.failed != nil {
		return e.aof.failed
	}
	if err := e.writeAOFBuffer(false); err != nil {
		return err
	}
	syncDue := closing || fsync == FsyncAlways ||
		(fsync == FsyncEverySec && time.Since(e.aof.lastSync) >= time.Second)
	if e.aof.dirty && syncDue && e.aof.syncPending == nil {
		// A rewrite's rename whose directory sync failed is finished first:
		// what is about to be synced is in the file that rename named.
		if err := e.syncPendingLogDir(); err != nil {
			e.aof.failed = err
			return err
		}
		if !closing && (fsync == FsyncEverySec || syncedOnly) {
			result := make(chan error, 1)
			file, syncFile, stats := e.aof.file, e.aofSync, &e.appendSyncStats
			wake := e.rewriteWake
			e.aof.syncPending = result
			e.aof.syncOffset = e.appendWritten
			e.aof.lastSync = time.Now()
			// Writes during this Sync stay dirty and require another sync.
			e.aof.dirty = false
			go func() {
				result <- timedPersistenceSync(stats, file, syncFile)
				if wake != nil {
					wake()
				}
			}()
			e.publishFlushed(syncedOnly)
			return nil
		}
		if err := timedPersistenceSync(&e.appendSyncStats, e.aof.file, e.aofSync); err != nil {
			e.aof.failed = err
			return err
		}
		e.aof.lastSync = time.Now()
		e.aof.dirty = false
		e.appendSynced = e.appendWritten
	}
	e.publishFlushed(syncedOnly)
	return nil
}

// publishFlushed advances the offset whose replies may be released: to what
// is written, or, when syncedOnly, to what a sync has covered.
func (e *Engine) publishFlushed(syncedOnly bool) {
	if syncedOnly {
		e.appendCompleted = e.appendSynced
		return
	}
	e.appendCompleted = e.appendWritten
}

// LoadAOF replays a log into the keyspace.
//
// A crash can leave a half-written command at the end - the process died
// between two write syscalls, or the filesystem kept only part of the last one.
// That tail is dropped rather than treated as corruption, because it is the
// expected shape of an unclean stop and the commands before it are perfectly
// good. Anything malformed earlier in the file is a real error and is reported
// as one.
//
// A transaction's block is held until its EXEC has been read and only then
// replayed, so a crash part way through writing one leaves none of it applied.
// Its MULTI is where the torn tail starts: what was intact of the block goes to
// the backup with the rest, which is Redis's rule for an AOF that ends inside
// MULTI.
//
// The log replays into e's keyspace, with eviction and expiry held off in e's
// space until it has.
func (e *Engine) LoadAOF(path string) (int, error) {
	return e.loadAOF(context.Background(), path)
}

// replayCheckEvery is how many records a replay reads between looks at its
// context: often enough that a cancelled Open stops within a millisecond or
// so, and rarely enough that looking costs nothing a replay would notice.
const replayCheckEvery = 1024

// loadAOF is LoadAOF, stopping when ctx is done. It looks at ctx before every
// replayCheckEvery-th record that is not inside a transaction's block, so a
// block is still replayed whole or not at all. Stopped, it returns ctx's
// error, wrapped with the log's path and where it stopped; the keyspace holds
// whatever was replayed until then, and nothing has been written anywhere, so
// the caller discards the engine and the log is as it was.
func (e *Engine) loadAOF(ctx context.Context, path string) (int, error) {
	e.aof.recovered = nil
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	// What the replay runs is not counted, but for the commands of a MULTI
	// block: Redis's replay runs each command bypassing call(), which is where
	// it counts commands, except that EXEC runs its block through call()
	// (aof.c's loadSingleAppendOnlyFile, multi.c's execCommand).
	totals := e.totals
	totals.errorPrefixes = maps.Clone(e.totals.errorPrefixes)
	inBlocks := uint64(0)
	// commandstats and the latency histograms are left as they were, a MULTI
	// block's commands' too: Redis's call() updates them only outside the
	// loading of its log (update_command_stats, isAOFLoadingContext), though
	// it counts every call in total_commands_processed. The replay records
	// its latencies in histograms of its own, which are dropped.
	stats := slices.Clone(e.cmdStats)
	latency := e.latency
	e.latency = make([]*latencyHistogram, len(latency))
	slow := e.slowlog
	defer func() {
		copy(e.cmdStats, stats)
		e.latency = latency
		// Nor does Redis's slow log hear of a command its replay runs.
		e.slowlog = slow
	}()
	defer func() {
		// Hits and misses stay as the replay left them: Redis's lookupKey
		// counts them whoever looks, its replay included (PFMERGE, which
		// it logs, reads its keys).
		hits, misses := e.totals.hits, e.totals.misses
		e.totals = totals
		e.totals.commands += inBlocks
		e.totals.hits, e.totals.misses = hits, misses
	}()
	e.aof.replaying = true
	e.space.SuspendEviction = true
	e.space.SuspendExpiry = true
	stopped := false
	defer func() {
		if stopped {
			// The engine is discarded: there is no point expiring or
			// evicting what was replayed of a log that will be replayed again.
			e.aof.replaying = false
			return
		}
		priorRemovalHook := e.space.OnRemove
		e.space.OnRemove = func(_, key string) { e.aof.recovered = append(e.aof.recovered, key) }
		defer func() { e.space.OnRemove = priorRemovalHook }()
		if e.replicaOf() != "" && e.replicationProtocol() == 2 {
			e.aof.replaying = false
			return // preserve the exact primary-decided prefix for checkpoints
		}
		e.space.SuspendExpiry = false
		// The keys whose TTL passed while the server was down, which Redis's
		// expiry cycle would remove after its load, and count, as this does.
		e.space.EachKeyspace(func(ks data_structure.Keyspace) {
			_, expired := ks.ActiveExpire(ks.KeysWithExpiry())
			e.expiredKeys += uint64(expired)
		})
		e.aof.replaying = false
		e.space.SuspendEviction = false
		e.space.EnforceLimits()
	}()
	reader := bufio.NewReaderSize(f, 64*1024)
	applied, used := 0, int64(0)
	// block is the open transaction's commands and where each began; begun is
	// where its MULTI began, or -1 outside one.
	var block []replayedCommand
	begun := int64(-1)
	for records := 0; ; records++ {
		if begun < 0 && records%replayCheckEvery == 0 {
			if err := ctx.Err(); err != nil {
				stopped = true
				return applied, fmt.Errorf("replay of %s stopped at byte %d: %w", path, used, err)
			}
		}
		cmd, n, err := readAOFCommand(reader)
		if err == io.EOF && n == 0 {
			if begun >= 0 {
				return applied, &truncatedAOF{path, begun, true}
			}
			return applied, nil
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			if begun >= 0 {
				return applied, &truncatedAOF{path, begun, true}
			}
			return applied, &truncatedAOF{path, used, false}
		}
		if err != nil {
			return applied, fmt.Errorf("malformed command at byte %d: %w", used, err)
		}
		frame := cmd.Cmd == "MULTI" || cmd.Cmd == "EXEC"
		switch {
		case frame && len(cmd.Args) != 0:
			return applied, fmt.Errorf("malformed %s frame at byte %d", cmd.Cmd, used)
		case cmd.Cmd == "MULTI" && begun >= 0:
			return applied, fmt.Errorf("MULTI at byte %d inside the transaction begun at byte %d", used, begun)
		case cmd.Cmd == "MULTI":
			begun = used
		case cmd.Cmd == "EXEC" && begun < 0:
			return applied, fmt.Errorf("EXEC without MULTI at byte %d", used)
		case cmd.Cmd == "EXEC":
			for _, queued := range block {
				if err := e.replayAOFCommand(queued.cmd, queued.at); err != nil {
					return applied, err
				}
				applied++
				inBlocks++
			}
			clear(block)
			block, begun = block[:0], -1
		case begun >= 0:
			block = append(block, replayedCommand{cmd, used})
		default:
			if err := e.replayAOFCommand(cmd, used); err != nil {
				return applied, err
			}
			applied++
		}
		used += n
	}
}

type replayedCommand struct {
	cmd *Command
	at  int64
}

func (e *Engine) replayAOFCommand(cmd *Command, at int64) error {
	sink := &replayWriter{}
	if err := e.EvalAndResponse(cmd, sink); err != nil {
		return fmt.Errorf("replaying %s at byte %d: %w", cmd.Cmd, at, err)
	}
	if sink.err != nil {
		return fmt.Errorf("replaying %s at byte %d: %w", cmd.Cmd, at, sink.err)
	}
	return nil
}

// Read one canonical AOF frame; memory is proportional to one command, not the log.
func readAOFCommand(r *bufio.Reader) (*Command, int64, error) {
	var consumed int64
	length := func(prefix byte) (int64, error) {
		line, err := r.ReadSlice('\n')
		consumed += int64(len(line))
		if err != nil {
			return 0, err
		}
		if len(line) < 4 || line[0] != prefix || line[len(line)-2] != '\r' {
			return 0, ErrProtocol
		}
		n, ok := parseDecimal(line[1 : len(line)-2])
		if !ok || n < 0 {
			return 0, ErrProtocol
		}
		return n, nil
	}
	count, err := length('*')
	if err != nil {
		return nil, consumed, err
	}
	if count == 0 || count > maxMultiBulkLength {
		return nil, consumed, ErrProtocol
	}
	parts := make([]string, 0, min(int(count), 16))
	for i := int64(0); i < count; i++ {
		size, err := length('$')
		if err != nil {
			return nil, consumed, err
		}
		if size > maxBulkLength {
			return nil, consumed, ErrProtocol
		}
		b := make([]byte, size+2)
		n, err := io.ReadFull(r, b)
		consumed += int64(n)
		if err != nil {
			return nil, consumed, err
		}
		if b[size] != '\r' || b[size+1] != '\n' {
			return nil, consumed, ErrProtocol
		}
		parts = append(parts, string(b[:size]))
	}
	return &Command{Cmd: strings.ToUpper(parts[0]), Args: parts[1:]}, consumed, nil
}

type replayWriter struct{ err error }

func (w *replayWriter) Read([]byte) (int, error) { return 0, io.EOF }
func (w *replayWriter) Write(p []byte) (int, error) {
	if len(p) > 0 && p[0] == '-' {
		w.err = errors.New(strings.TrimSpace(string(p)))
	}
	return len(p), nil
}

type truncatedAOF struct {
	path   string
	offset int64
	// transaction says the tail begins with a transaction whose EXEC was
	// never written, rather than with a command cut short.
	transaction bool
}

func (e *truncatedAOF) Error() string {
	if e.transaction {
		return fmt.Sprintf("incomplete final transaction in %s from byte %d", e.path, e.offset)
	}
	return fmt.Sprintf("truncated final command in %s at byte %d", e.path, e.offset)
}
func (e *truncatedAOF) Unwrap() error { return errTruncatedAOF }

// RepairAOFTail preserves the torn suffix before truncating to the last
// complete command, or to before the MULTI of a transaction that never ended.
func RepairAOFTail(cause error) error {
	var tail *truncatedAOF
	if !errors.As(cause, &tail) {
		return cause
	}
	f, err := os.OpenFile(tail.path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	backup, err := os.CreateTemp(filepath.Dir(tail.path), ".keel-torn-tail-*")
	if err != nil {
		return err
	}
	_, err = f.Seek(tail.offset, io.SeekStart)
	if err == nil {
		_, err = io.Copy(backup, f)
	}
	if err == nil {
		err = backup.Sync()
	}
	closeErr := backup.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(tail.path)); err != nil {
		return err
	}
	if err := f.Truncate(tail.offset); err != nil {
		return err
	}
	return f.Sync()
}

// errTruncatedAOF marks the one failure that is not a failure: a log whose last
// command did not finish being written.
var errTruncatedAOF = errors.New("truncated final command")

// IsTruncatedAOF reports whether a LoadAOF error is only a half-written tail,
// which a server should log and carry on from rather than refuse to start over.
func IsTruncatedAOF(err error) bool { return errors.Is(err, errTruncatedAOF) }

// discardWriter throws replies away. Replaying produces one per command and
// there is nobody to send them to.
type discardWriter struct{}

func (discardWriter) Read([]byte) (int, error)    { return 0, io.EOF }
func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// aofExpireAt stages a PEXPIREAT for a key whose TTL was just set relative to
// now, so the log names an instant rather than a duration.
func (e *Engine) aofExpireAt(key string) {
	if e.aof.file == nil || e.aof.replaying {
		return
	}
	owner, ok := e.space.OwnerOf(key)
	if !ok {
		return
	}
	at, has := owner.GetExpiry(key)
	if !has {
		return
	}
	e.aofRecord("PEXPIREAT", key, strconv.FormatUint(at, 10))
}
