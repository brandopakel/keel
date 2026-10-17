package core

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/brandopakel/keel/internal/constant"
)

// Transactions: MULTI, EXEC and DISCARD.
//
// A connection that sends MULTI stops executing what it sends and queues it
// instead, answering QUEUED for each command, until EXEC runs the queue in one
// go or DISCARD throws it away. That much is what every client library's
// transaction API expects. The rest of Redis's contract is kept as Redis words
// it, because clients match on the error strings:
//
//   - A command refused while it is being queued - unknown, given the wrong
//     number of arguments, not allowed in a transaction, or refused by a
//     replica - is answered with that error and dooms the transaction. EXEC
//     then answers EXECABORT and runs nothing.
//   - A command that fails while EXEC runs it is an error inside EXEC's reply,
//     and the commands after it still run. There is no rollback, in Redis or
//     here.
//   - MULTI inside MULTI is an error that leaves the transaction open. EXEC or
//     DISCARD outside one is an error.
//
// # Atomic because execution is serial
//
// EXEC is a single command to the event loop, and it runs every queued command
// before it returns, on the loop's own thread. Nothing another client sends can
// run between two of them: I/O threads only read, parse and write, and the
// concurrent-append mode lets a run overlap a pending append only when it can
// bound every command in the run, which it cannot do for EXEC - so EXEC waits
// at the drained barrier like every other command it does not model.
//
// # Atomic on disk and on a replica
//
// Serial execution keeps other clients out. It does nothing about a crash part
// way through writing the block, or a replica that has received half of one.
// So the block is framed: MULTI before its first record and EXEC after its
// last, in the log and in the protocol 2 stream, which is what Redis writes to
// its AOF too. Loading the log replays a block only once its EXEC has been
// read, and a crash that leaves one open is a torn tail like any other, cut back
// to before its MULTI. A replica holds a block back until its EXEC arrives and
// then applies it in one turn of its loop. A transaction that recorded nothing
// - one made only of reads - writes no frame at all, because the log grows with
// changes, not with traffic.
//
// WATCH is not implemented and stays an unknown command, so a client's
// optimistic-locking API fails loudly instead of quietly not watching. Inside
// MULTI it gets Redis's own refusal, which leaves the transaction open. UNWATCH
// answers as Redis answers it when nothing is watched, which is always here.

// maxTransactionBytes bounds what one connection may hold queued, measured the
// way parsed input is measured for the buffer limits. It is the per-client
// limit on incomplete input as well: a queued command is input that has arrived
// and not yet run. A command that would pass it is refused like any other
// command refused while queueing, which aborts the transaction, so a client
// that keeps sending learns why at EXEC rather than growing the queue.
//
// It also keeps a transaction's replication block well inside the history a
// replica could receive it from; see replicationTransactionBytes.
const maxTransactionBytes = 16 << 20

// The replies clients match on, worded as Redis words them.
var (
	queuedReply    = []byte("+QUEUED\r\n")
	execAbortReply = []byte("-EXECABORT Transaction discarded because of previous errors.\r\n")

	errNestedMulti         = errors.New("ERR MULTI calls can not be nested")
	errExecNoMulti         = errors.New("ERR EXEC without MULTI")
	errDiscardNoMulti      = errors.New("ERR DISCARD without MULTI")
	errNotInTransaction    = errors.New("ERR Command not allowed inside a transaction")
	errWatchInMulti        = errors.New("ERR WATCH inside MULTI is not allowed")
	errTransactionTooLarge = fmt.Errorf("ERR transaction exceeds the %d MiB queued command limit", maxTransactionBytes>>20)
	errNoReply             = errors.New("ERR command produced no reply")
	// ErrTransactionReplyTooLarge is returned to the transport, not to the
	// client: the transaction has run, and its reply cannot be delivered.
	ErrTransactionReplyTooLarge = errors.New("output buffer limit exceeded")
)

// notInTransaction are commands with no meaning as part of a transaction, and
// refused inside one with Redis's error for that. A replication pull serves the
// stream as it stands between commands, can start a snapshot rewrite and can
// fence the node, and Redis refuses its own replication commands, SYNC and
// PSYNC, inside MULTI the same way. Changing terms has no Redis counterpart:
// it acts on the node rather than the data, and is made durable outside the
// log. Everything else Redis queues is queued here, BGREWRITEAOF and AUTH
// included.
var notInTransaction = map[string]bool{
	"KEEL.PROMOTE": true, "KEEL.FENCE": true,
	"KEEL.REPL.PULL": true, "KEEL.REPL.PULL2": true,
}

// layoutPadUnwatch is layout padding for a throwaway control build: never run.
//
//go:noinline
func layoutPadUnwatch(s *[64]uint64) {
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
}

// cmdUNWATCH forgets every watched key. Nothing is ever watched, so it only
// answers, as Redis answers it then. Clients send it when they finish with a
// connection that may have watched something: go-redis's Tx.Close and
// redis-py's pipeline reset both do.
func (e *Engine) cmdUNWATCH(args []string) []byte {
	if len(args) != 0 {
		return e.encode(wrongArguments("UNWATCH"), false)
	}
	return constant.RespOk
}

// Connection is the transport's part in a transaction: the commands it answers
// itself, about the connection rather than the data - connectionCommands -
// which the command table does not hold. Redis queues AUTH and its kind like
// any other command and runs each in its place at EXEC, and so does this.
type Connection interface {
	// AnswerConnection runs one such command and writes its reply.
	AnswerConnection(cmd *Command, w io.ReadWriter)
	// RESP3 reports whether the connection speaks RESP3 at this moment, which
	// a HELLO run earlier in the same EXEC may have just changed.
	RESP3() bool
}

// answers reports whether conn, which may be nil, answers name itself.
func answers(conn Connection, name string) bool {
	return conn != nil && connectionCommands[name]
}

// Transaction is one connection's open MULTI. The connection keeps a pointer
// to it that is nil the rest of the time, so a connection outside a
// transaction carries nothing for it.
type Transaction struct {
	commands []*Command
	bytes    int
	// aborted is set once a command has been refused while queueing. Nothing
	// queued after that will ever run, so nothing more is kept.
	aborted bool
}

// RetainedBytes is what the queue holds, for the transport's buffer accounting.
func (tx *Transaction) RetainedBytes() int {
	if tx == nil {
		return 0
	}
	return tx.bytes
}

// queueSlotBytes is charged per queued command for its place in the queue: a
// pointer, and as much again for the slack a growing slice keeps.
const queueSlotBytes = 16

// CommandRetainedBytes estimates what one parsed command holds: the struct,
// its name as upper-cased and as sent, its argument headers and their bytes.
// Parsed input and queued commands are charged by the same estimate, so moving
// a command from one to the other neither creates nor hides memory.
func CommandRetainedBytes(cmd *Command) int {
	used := 64 + len(cmd.Cmd) + len(cmd.Name) + cap(cmd.Args)*16
	for _, arg := range cmd.Args {
		used += len(arg)
	}
	return used
}

// IsTransactionCommand reports whether name is MULTI, EXEC or DISCARD, which a
// connection must hand to Transact whether or not it has a transaction open.
func IsTransactionCommand(name string) bool {
	return name == "MULTI" || name == "EXEC" || name == "DISCARD"
}

// Transact answers cmd on e for a connection whose open transaction is tx,
// nil if it has none, and returns the transaction that is open afterwards:
// what EXEC runs, it runs on e. The transport calls it for every command
// while one is open, and for MULTI, EXEC and DISCARD always; conn, which may
// be nil, answers the commands it handles itself when EXEC reaches them.
//
// Like EvalAndResponse, the error is the connection's rather than the
// command's: ErrTransactionReplyTooLarge means a transaction ran and its reply
// cannot be delivered, so the connection has to be closed, which is what any
// reply over the output limit already costs.
func (e *Engine) Transact(tx *Transaction, cmd *Command, w io.ReadWriter, conn Connection) (*Transaction, error) {
	if !IsTransactionCommand(cmd.Cmd) && tx == nil {
		return nil, e.EvalAndResponse(cmd, w)
	}
	// Everything written from here on is the transaction's own reply, and its
	// errors are counted as they are written (countedWriter).
	out := countedWriter{e, w}
	if !IsTransactionCommand(cmd.Cmd) {
		if cmd.Cmd == "WATCH" {
			// WATCH is not implemented, and outside a transaction it is an
			// unknown command. Inside one Redis refuses it without aborting
			// the transaction, after counting its arguments as it counts any
			// command's, and clients get the same answers here.
			reply := e.encode(errWatchInMulti, false)
			if len(cmd.Args) == 0 {
				reply = tx.refuse(wrongArguments("WATCH"))
				e.NoteCommandRefused(cmd)
			} else {
				// Redis runs WATCH inside a transaction rather than queue it,
				// and it is its refusal that answers.
				defer e.NoteCommandRan(cmd, e.StartCommand())
			}
			_, err := out.Write(reply)
			return tx, err
		}
		reply := tx.queue(e, cmd, conn)
		if len(reply) > 0 && reply[0] == '-' {
			// Refused while queueing, as Redis refuses it before it runs.
			e.NoteCommandRefused(cmd)
		}
		_, err := out.Write(reply)
		return tx, err
	}
	if err := CommandError(cmd); err != nil {
		// Redis counts arguments before it looks at what a command does, so a
		// malformed MULTI, EXEC or DISCARD is refused as any command would be:
		// inside a transaction that aborts it, and a malformed EXEC answers
		// EXECABORT, discarding the transaction if there is one.
		e.NoteCommandRefused(cmd)
		if cmd.Cmd == "EXEC" {
			_, err := out.Write(Refusal(cmd, err))
			return nil, err
		}
		_, err := out.Write(tx.refuse(err))
		return tx, err
	}
	// MULTI, EXEC and DISCARD run, so they count, their refusals of a
	// connection in the wrong state included, as Redis finds those inside
	// the command. What EXEC runs counts as it runs, and EXEC after it, its
	// time theirs and its own together, as Redis times it.
	start := e.StartCommand()
	var reply []byte
	switch {
	case cmd.Cmd == "MULTI" && tx != nil:
		reply = e.encode(errNestedMulti, false)
	case cmd.Cmd == "MULTI":
		tx, reply = &Transaction{}, constant.RespOk
	case tx == nil:
		reply = e.encode(errExecNoMulti, false)
		if cmd.Cmd == "DISCARD" {
			reply = e.encode(errDiscardNoMulti, false)
		}
	case cmd.Cmd == "DISCARD":
		tx, reply = nil, constant.RespOk
	default:
		// EXEC fails only for its own refusal, EXECABORT: what its commands
		// answered is theirs, as Redis's call() counts it.
		aborted, err := tx.exec(e, out, conn)
		e.noteRun(cmd, statOf(cmd), start.started, aborted)
		return nil, err
	}
	_, err := out.Write(reply)
	e.NoteCommandRan(cmd, start)
	return tx, err
}

// countedWriter counts each error written through it toward
// total_error_replies and errorstats. A transaction writes each of its own
// replies in one call; EXEC's reply is an array, whose errors were counted as
// the commands that answered them ran.
type countedWriter struct {
	e *Engine
	w io.Writer
}

func (c countedWriter) Write(p []byte) (int, error) {
	if len(p) > 0 && p[0] == '-' {
		c.e.noteError(p)
	}
	return c.w.Write(p)
}

// refuse answers a command refused while queueing, which aborts the transaction.
func (tx *Transaction) refuse(err error) []byte {
	tx.abort()
	return Encode(err, false)
}

func (tx *Transaction) abort() {
	if tx == nil {
		return
	}
	tx.aborted = true
	clear(tx.commands)
	tx.commands, tx.bytes = nil, 0
}

// queue checks cmd as Redis does before queueing it, and holds it if it passes.
func (tx *Transaction) queue(e *Engine, cmd *Command, conn Connection) []byte {
	if err := e.queueRefusal(cmd, conn); err != nil {
		return tx.refuse(err)
	}
	if tx.aborted {
		return queuedReply
	}
	size := CommandRetainedBytes(cmd) + queueSlotBytes
	if size > maxTransactionBytes-tx.bytes {
		return tx.refuse(errTransactionTooLarge)
	}
	tx.commands = append(tx.commands, cmd)
	tx.bytes += size
	return queuedReply
}

// queueRefusal checks cmd in Redis's order: the name and the count of
// arguments, then whether a transaction may hold it, then whether this node
// may run it.
func (e *Engine) queueRefusal(cmd *Command, conn Connection) error {
	if err := CommandError(cmd); err != nil {
		return err
	}
	if answers(conn, cmd.Cmd) {
		// The transport's own commands are not about the data, so neither a
		// replica nor a fenced primary refuses them.
		return nil
	}
	if commands[cmd.Cmd].run == nil {
		// A connection command with no transport to answer it.
		return unknownCommand(cmd)
	}
	if notInTransaction[cmd.Cmd] {
		return errNotInTransaction
	}
	// A replica refuses writes, and reads once it has lost its primary, as it
	// would outside a transaction; and so does a primary that has been fenced.
	return e.replicaCommandError(cmd.Cmd)
}

// execAbort is EXEC's refusal, which discards the transaction and names the
// reason. Redis drops the generic ERR prefix from the reason and keeps any
// other class, such as NOAUTH, READONLY or MASTERDOWN, which is the part a
// client can act on.
func execAbort(cause error) []byte {
	reason := strings.TrimPrefix(cause.Error(), "ERR ")
	return Encode(fmt.Errorf("EXECABORT Transaction discarded because of: %s", reason), false)
}

func writeExecAbort(w io.Writer, cause error) error {
	_, err := w.Write(execAbort(cause))
	return err
}

// transactionReplySlack is kept back for each command still to run when sizing
// what a large reply inside EXEC may use, so that a large read early in a
// transaction does not leave the small replies after it with nowhere to go. It
// covers an integer, a status or a typical error.
const transactionReplySlack = 128

// exec runs tx on e, in e's command scope.
//
// aborted is whether EXEC answered EXECABORT and ran nothing.
func (tx *Transaction) exec(e *Engine, w io.Writer, conn Connection) (aborted bool, err error) {
	if tx.aborted {
		_, err := w.Write(execAbortReply)
		return true, err
	}
	// What was allowed when a command was queued may not be allowed now: the
	// node may have been fenced by a higher term since, or a replica may have
	// lost its primary. Either all of a transaction runs or none of it does, so
	// every command is checked before the first one runs, as Redis checks EXEC.
	for _, cmd := range tx.commands {
		// PING is refused for a failed log when it arrives, so that a health
		// check sees the failure, but Redis checks only EXEC's writes when EXEC
		// comes (server.c 4547-4548).
		if answers(conn, cmd.Cmd) || cmd.Cmd == "PING" && e.logFailure.failing {
			continue
		}
		if err := e.replicaCommandError(cmd.Cmd); err != nil {
			return true, writeExecAbort(w, err)
		}
	}

	// The reply is one array of every command's reply, and it is bound by the
	// same output limit as any other reply. Amplifying reads are told how much
	// of it the replies before them left, through e.replyCeiling, so one that
	// would not fit is refused before it is built. A reply that still does not
	// fit - many small ones - cannot be delivered, and the transaction runs on
	// regardless, because half of one must never run: the replies are dropped
	// and the transport closes the connection.
	n := len(tx.commands)
	total := decimalDigits(n) + 3
	replies := make([][]byte, 0, n)
	overflow := false
	ceiling := func(ran int) int {
		return max(0, MaxReplyBytes-total-transactionReplySlack*max(0, n-ran-1))
	}
	// Execution inside EXEC grows the log and holds replies just as a
	// pipelined run grows the arena; later commands' reservations must see both.
	budget := e.commandAllocations
	var retained, logged int
	if budget != nil {
		retained, logged = budget.Retained, e.AppendRetainedBytes()
	}
	e.replyCeiling = ceiling(0)
	defer func() { e.replyCeiling = MaxReplyBytes }()
	run := func(cmd *Command, sink io.ReadWriter) error {
		if answers(conn, cmd.Cmd) {
			conn.AnswerConnection(cmd, sink)
			return nil
		}
		// Each reply is framed in the protocol the connection has when the
		// command runs, not when it was queued: Redis runs a queued HELLO in
		// its place, and frames every reply after it in the new protocol.
		cmd.RESP3 = conn != nil && conn.RESP3()
		return e.EvalAndResponse(cmd, sink)
	}
	e.runTransaction(tx.commands, run, func(i int, reply []byte, err error) {
		if err == nil && len(reply) == 0 {
			// An empty element would shift every reply after it, and the
			// client would read each as the answer to the command before.
			err = errNoReply
			e.noteError(Encode(err, false))
		}
		if err != nil {
			reply = Encode(err, false)
		}
		if overflow || len(reply) > MaxReplyBytes-total {
			overflow, replies = true, nil
		} else {
			total += len(reply)
			replies = append(replies, reply)
		}
		if budget != nil {
			budget.ObserveRetained(retained + max(0, e.AppendRetainedBytes()-logged) + total)
		}
		e.replyCeiling = ceiling(i + 1)
	})
	if overflow {
		return false, ErrTransactionReplyTooLarge
	}
	out := appendArrayHeader(make([]byte, 0, total), n)
	for _, reply := range replies {
		out = append(out, reply...)
	}
	_, err = w.Write(out)
	return false, err
}

// runTransaction runs commands on e as one unit through run, for EXEC and for
// a block a replica has received. The unit is framed in the log and in the
// protocol 2 stream, and eviction in e's space waits until the block is
// closed, so a block holds what its transaction did and not the removal of
// whatever unrelated key the budget chose. Redis evicts before EXEC rather
// than between its commands, for the same reason; the budget can be passed by
// up to one transaction meanwhile, which the queue limit bounds. reply hears
// each command's answer in order.
func (e *Engine) runTransaction(commands []*Command, run func(*Command, io.ReadWriter) error, reply func(int, []byte, error)) {
	e.aof.transaction, e.aof.transactionLogged = true, false
	e.replicationTransaction = replicationBlock{active: true}
	suspended := e.space.SuspendEviction
	e.space.SuspendEviction = true
	for i, cmd := range commands {
		var sink transactionSink
		err := run(cmd, &sink)
		reply(i, sink.p, err)
	}
	e.closeAOFTransaction()
	e.closeReplicationTransaction()
	e.space.SuspendEviction = suspended
	e.space.EnforceLimits()
}

// transactionSink keeps the reply it is handed rather than copying it, as the
// transport's own capture does; a command writes its reply once.
type transactionSink struct{ p []byte }

func (s *transactionSink) Read([]byte) (int, error) { return 0, io.EOF }
func (s *transactionSink) Write(p []byte) (int, error) {
	if s.p == nil {
		s.p = p
	} else {
		s.p = append(append(make([]byte, 0, len(s.p)+len(p)), s.p...), p...)
	}
	return len(p), nil
}

// The frames around a transaction's block, in the log and the protocol 2
// stream alike.
var (
	transactionOpenFrame  = appendCommand(nil, "MULTI")
	transactionCloseFrame = appendCommand(nil, "EXEC")
)

// openAOFTransaction writes MULTI ahead of a transaction's first record. It is
// the log's frame, not part of the command whose record follows it: protocol 2
// publishes its own MULTI when the block's first body reaches the history, and
// an opaque command publishes an image rather than its log bytes at all. So the
// frame is written as if outside a command, where a bounded drain does not
// publish it, and the command's published range starts after it. It is the
// first record of the block, so nothing of this command precedes it.
func (e *Engine) openAOFTransaction() {
	active := e.aof.commandActive
	e.aof.commandActive = false
	e.appendAOFCommand("MULTI")
	e.aof.commandActive = active
	e.aof.commandStart = len(e.aof.buf)
}

// closeAOFTransaction writes EXEC after the last record of a block that has
// one, between commands.
func (e *Engine) closeAOFTransaction() {
	logged := e.aof.transactionLogged
	e.aof.transaction, e.aof.transactionLogged = false, false
	if logged && e.aof.file != nil && !e.aof.replaying {
		e.appendAOFCommand("EXEC")
	}
}

// replicationBlock is the protocol 2 framing of the transaction running now.
//
// A replica has to hold a block until its EXEC arrives, and can only be sent
// it from the history, so a block the history cannot hold whole could never
// arrive whole: the stream is invalidated instead, and every replica takes a
// snapshot that already contains the transaction. dropped records that, and
// keeps the rest of the block out of the new epoch's history, which no replica
// is served from before the snapshot is taken.
type replicationBlock struct {
	active, opened, dropped bool
	bytes                   int
}

// replicationTransactionBytes is the most of one block the stream will carry.
// Anything larger is past the history's own limit before it has ended.
const replicationTransactionBytes = replicationHistoryLimit

// admitReplicationTransaction frames a body published inside a transaction.
func (e *Engine) admitReplicationTransaction(n int) bool {
	block := &e.replicationTransaction
	if block.dropped {
		return false
	}
	if !block.opened {
		block.opened, block.bytes = true, len(transactionOpenFrame)
		e.appendReplicationV2History(transactionOpenFrame)
	}
	if n > replicationTransactionBytes-block.bytes-len(transactionCloseFrame) {
		e.invalidateReplicationV2()
		return false
	}
	block.bytes += n
	return true
}

func (e *Engine) closeReplicationTransaction() {
	block := e.replicationTransaction
	e.replicationTransaction = replicationBlock{}
	if !block.opened || block.dropped || !e.replicationV2Enabled() {
		return
	}
	if e.aof.failed != nil {
		// The bodies after the failure were never published. Ending the block
		// would deliver part of a transaction as all of it.
		e.invalidateReplicationV2()
		return
	}
	e.appendReplicationV2History(transactionCloseFrame)
}

// MayReplicate reports whether cmd writes or may be replicated, as Redis's
// processCommand decides it for CLIENT PAUSE WRITE (is_may_replicate_command):
// its spec declares WRITE or MAY_REPLICATE, a container's subcommand by its
// own spec. A command Keel does not have is neither.
func MayReplicate(cmd *Command) bool {
	name := cmd.Cmd
	if subcommandsOf(name) != nil && len(cmd.Args) > 0 {
		name += "|" + strings.ToUpper(cmd.Args[0])
	}
	spec, ok := commandSpecOf(name)
	return ok && (spec.hasFlag("WRITE") || spec.hasFlag("MAY_REPLICATE"))
}

// MayReplicate reports whether a command tx has queued writes or may be
// replicated, which makes its EXEC one that CLIENT PAUSE WRITE holds.
func (tx *Transaction) MayReplicate() bool {
	if tx == nil {
		return false
	}
	for _, cmd := range tx.commands {
		if MayReplicate(cmd) {
			return true
		}
	}
	return false
}
