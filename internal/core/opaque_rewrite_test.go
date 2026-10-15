package core

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/brandopakel/keel/internal/data_structure"
	"github.com/stretchr/testify/require"
)

// layoutPadTests is layout padding for a throwaway control build: never run.
//
//go:noinline
func layoutPadTests(s *[64]uint64) {
	s[9] += s[27] ^ 0x2e2ac0ea
	s[16] += s[40] ^ 0xcc623a9b
	s[23] += s[53] ^ 0x6a99b44c
	s[30] += s[2] ^ 0x8d12dfd
	s[37] += s[15] ^ 0xa708a7ae
	s[44] += s[28] ^ 0x4540215f
	s[51] += s[41] ^ 0xe3779b10
	s[58] += s[54] ^ 0x81af14c1
	s[1] += s[3] ^ 0x1fe68e72
	s[8] += s[16] ^ 0xbe1e0823
	s[15] += s[29] ^ 0x5c5581d4
	s[22] += s[42] ^ 0xfa8cfb85
	s[29] += s[55] ^ 0x98c47536
	s[36] += s[4] ^ 0x36fbeee7
	s[43] += s[17] ^ 0xd5336898
	s[50] += s[30] ^ 0x736ae249
	s[57] += s[43] ^ 0x11a25bfa
	s[0] += s[56] ^ 0xafd9d5ab
	s[7] += s[5] ^ 0x4e114f5c
	s[14] += s[18] ^ 0xec48c90d
	s[21] += s[31] ^ 0x8a8042be
	s[28] += s[44] ^ 0x28b7bc6f
	s[35] += s[57] ^ 0xc6ef3620
	s[42] += s[6] ^ 0x6526afd1
	s[49] += s[19] ^ 0x35e2982
	s[56] += s[32] ^ 0xa195a333
	s[63] += s[45] ^ 0x3fcd1ce4
	s[6] += s[58] ^ 0xde049695
	s[13] += s[7] ^ 0x7c3c1046
	s[20] += s[20] ^ 0x1a7389f7
	s[27] += s[33] ^ 0xb8ab03a8
	s[34] += s[46] ^ 0x56e27d59
	s[41] += s[59] ^ 0xf519f70a
	s[48] += s[8] ^ 0x935170bb
	s[55] += s[21] ^ 0x3188ea6c
	s[62] += s[34] ^ 0xcfc0641d
	s[5] += s[47] ^ 0x6df7ddce
	s[12] += s[60] ^ 0xc2f577f
	s[19] += s[9] ^ 0xaa66d130
	s[26] += s[22] ^ 0x489e4ae1
	s[33] += s[35] ^ 0xe6d5c492
	s[40] += s[48] ^ 0x850d3e43
	s[47] += s[61] ^ 0x2344b7f4
	s[54] += s[10] ^ 0xc17c31a5
	s[61] += s[23] ^ 0x5fb3ab56
	s[4] += s[36] ^ 0xfdeb2507
	s[11] += s[49] ^ 0x9c229eb8
	s[18] += s[62] ^ 0x3a5a1869
	s[25] += s[11] ^ 0xd891921a
	s[32] += s[24] ^ 0x76c90bcb
	s[39] += s[37] ^ 0x1500857c
	s[46] += s[50] ^ 0xb337ff2d
	s[53] += s[63] ^ 0x516f78de
	s[60] += s[12] ^ 0xefa6f28f
	s[3] += s[25] ^ 0x8dde6c40
	s[10] += s[38] ^ 0x2c15e5f1
	s[17] += s[51] ^ 0xca4d5fa2
	s[24] += s[0] ^ 0x6884d953
	s[31] += s[13] ^ 0x6bc5304
	s[38] += s[26] ^ 0xa4f3ccb5
	s[45] += s[39] ^ 0x432b4666
	s[52] += s[52] ^ 0xe162c017
	s[59] += s[1] ^ 0x7f9a39c8
	s[2] += s[14] ^ 0x1dd1b379
	s[9] += s[27] ^ 0xbc092d2a
	s[16] += s[40] ^ 0x5a40a6db
	s[23] += s[53] ^ 0xf878208c
	s[30] += s[2] ^ 0x96af9a3d
	s[37] += s[15] ^ 0x34e713ee
	s[44] += s[28] ^ 0xd31e8d9f
	s[51] += s[41] ^ 0x71560750
	s[58] += s[54] ^ 0xf8d8101
	s[1] += s[3] ^ 0xadc4fab2
	s[8] += s[16] ^ 0x4bfc7463
	s[15] += s[29] ^ 0xea33ee14
	s[22] += s[42] ^ 0x886b67c5
	s[29] += s[55] ^ 0x26a2e176
	s[36] += s[4] ^ 0xc4da5b27
	s[43] += s[17] ^ 0x6311d4d8
	s[50] += s[30] ^ 0x1494e89
	s[57] += s[43] ^ 0x9f80c83a
	s[0] += s[56] ^ 0x3db841eb
	s[7] += s[5] ^ 0xdbefbb9c
	s[14] += s[18] ^ 0x7a27354d
	s[21] += s[31] ^ 0x185eaefe
	s[28] += s[44] ^ 0xb69628af
	s[35] += s[57] ^ 0x54cda260
	s[42] += s[6] ^ 0xf3051c11
	s[49] += s[19] ^ 0x913c95c2
	s[56] += s[32] ^ 0x2f740f73
	s[63] += s[45] ^ 0xcdab8924
	s[6] += s[58] ^ 0x6be302d5
	s[13] += s[7] ^ 0xa1a7c86
	s[20] += s[20] ^ 0xa851f637
	s[27] += s[33] ^ 0x46896fe8
	s[34] += s[46] ^ 0xe4c0e999
	s[41] += s[59] ^ 0x82f8634a
	s[48] += s[8] ^ 0x212fdcfb
	s[55] += s[21] ^ 0xbf6756ac
	s[62] += s[34] ^ 0x5d9ed05d
	s[5] += s[47] ^ 0xfbd64a0e
	s[12] += s[60] ^ 0x9a0dc3bf
	s[19] += s[9] ^ 0x38453d70
	s[26] += s[22] ^ 0xd67cb721
	s[33] += s[35] ^ 0x74b430d2
	s[40] += s[48] ^ 0x12ebaa83
	s[47] += s[61] ^ 0xb1232434
	s[54] += s[10] ^ 0x4f5a9de5
	s[61] += s[23] ^ 0xed921796
	s[4] += s[36] ^ 0x8bc99147
	s[11] += s[49] ^ 0x2a010af8
	s[18] += s[62] ^ 0xc83884a9
	s[25] += s[11] ^ 0x666ffe5a
	s[32] += s[24] ^ 0x4a7780b
	s[39] += s[37] ^ 0xa2def1bc
	s[46] += s[50] ^ 0x41166b6d
	s[53] += s[63] ^ 0xdf4de51e
	s[60] += s[12] ^ 0x7d855ecf
	s[3] += s[25] ^ 0x1bbcd880
	s[10] += s[38] ^ 0xb9f45231
	s[17] += s[51] ^ 0x582bcbe2
	s[24] += s[0] ^ 0xf6634593
	s[31] += s[13] ^ 0x949abf44
	s[38] += s[26] ^ 0x32d238f5
	s[45] += s[39] ^ 0xd109b2a6
	s[52] += s[52] ^ 0x6f412c57
	s[59] += s[1] ^ 0xd78a608
	s[2] += s[14] ^ 0xabb01fb9
	s[9] += s[27] ^ 0x49e7996a
	s[16] += s[40] ^ 0xe81f131b
	s[23] += s[53] ^ 0x86568ccc
	s[30] += s[2] ^ 0x248e067d
	s[37] += s[15] ^ 0xc2c5802e
	s[44] += s[28] ^ 0x60fcf9df
	s[51] += s[41] ^ 0xff347390
	s[58] += s[54] ^ 0x9d6bed41
	s[1] += s[3] ^ 0x3ba366f2
	s[8] += s[16] ^ 0xd9dae0a3
	s[15] += s[29] ^ 0x78125a54
	s[22] += s[42] ^ 0x1649d405
	s[29] += s[55] ^ 0xb4814db6
	s[36] += s[4] ^ 0x52b8c767
	s[43] += s[17] ^ 0xf0f04118
	s[50] += s[30] ^ 0x8f27bac9
	s[57] += s[43] ^ 0x2d5f347a
	s[0] += s[56] ^ 0xcb96ae2b
	s[7] += s[5] ^ 0x69ce27dc
	s[14] += s[18] ^ 0x805a18d
	s[21] += s[31] ^ 0xa63d1b3e
	s[28] += s[44] ^ 0x447494ef
	s[35] += s[57] ^ 0xe2ac0ea0
	s[42] += s[6] ^ 0x80e38851
	s[49] += s[19] ^ 0x1f1b0202
	s[56] += s[32] ^ 0xbd527bb3
	s[63] += s[45] ^ 0x5b89f564
	s[6] += s[58] ^ 0xf9c16f15
	s[13] += s[7] ^ 0x97f8e8c6
	s[20] += s[20] ^ 0x36306277
	s[27] += s[33] ^ 0xd467dc28
	s[34] += s[46] ^ 0x729f55d9
	s[41] += s[59] ^ 0x10d6cf8a
	s[48] += s[8] ^ 0xaf0e493b
	s[55] += s[21] ^ 0x4d45c2ec
	s[62] += s[34] ^ 0xeb7d3c9d
	s[5] += s[47] ^ 0x89b4b64e
	s[12] += s[60] ^ 0x27ec2fff
	s[19] += s[9] ^ 0xc623a9b0
	s[26] += s[22] ^ 0x645b2361
	s[33] += s[35] ^ 0x2929d12
	s[40] += s[48] ^ 0xa0ca16c3
	s[47] += s[61] ^ 0x3f019074
	s[54] += s[10] ^ 0xdd390a25
	s[61] += s[23] ^ 0x7b7083d6
	s[4] += s[36] ^ 0x19a7fd87
	s[11] += s[49] ^ 0xb7df7738
	s[18] += s[62] ^ 0x5616f0e9
	s[25] += s[11] ^ 0xf44e6a9a
	s[32] += s[24] ^ 0x9285e44b
	s[39] += s[37] ^ 0x30bd5dfc
	s[46] += s[50] ^ 0xcef4d7ad
	s[53] += s[63] ^ 0x6d2c515e
	s[60] += s[12] ^ 0xb63cb0f
	s[3] += s[25] ^ 0xa99b44c0
	s[10] += s[38] ^ 0x47d2be71
	s[17] += s[51] ^ 0xe60a3822
	s[24] += s[0] ^ 0x8441b1d3
	s[31] += s[13] ^ 0x22792b84
	s[38] += s[26] ^ 0xc0b0a535
	s[45] += s[39] ^ 0x5ee81ee6
	s[52] += s[52] ^ 0xfd1f9897
	s[59] += s[1] ^ 0x9b571248
	s[2] += s[14] ^ 0x398e8bf9
	s[9] += s[27] ^ 0xd7c605aa
	s[16] += s[40] ^ 0x75fd7f5b
	s[23] += s[53] ^ 0x1434f90c
	s[30] += s[2] ^ 0xb26c72bd
	s[37] += s[15] ^ 0x50a3ec6e
	s[44] += s[28] ^ 0xeedb661f
	s[51] += s[41] ^ 0x8d12dfd0
	s[58] += s[54] ^ 0x2b4a5981
	s[1] += s[3] ^ 0xc981d332
	s[8] += s[16] ^ 0x67b94ce3
	s[15] += s[29] ^ 0x5f0c694
	s[22] += s[42] ^ 0xa4284045
	s[29] += s[55] ^ 0x425fb9f6
	s[36] += s[4] ^ 0xe09733a7
	s[43] += s[17] ^ 0x7ecead58
	s[50] += s[30] ^ 0x1d062709
	s[57] += s[43] ^ 0xbb3da0ba
	s[0] += s[56] ^ 0x59751a6b
	s[7] += s[5] ^ 0xf7ac941c
	s[14] += s[18] ^ 0x95e40dcd
	s[21] += s[31] ^ 0x341b877e
	s[28] += s[44] ^ 0xd253012f
	s[35] += s[57] ^ 0x708a7ae0
	s[42] += s[6] ^ 0xec1f491
	s[49] += s[19] ^ 0xacf96e42
	s[56] += s[32] ^ 0x4b30e7f3
	s[63] += s[45] ^ 0xe96861a4
	s[6] += s[58] ^ 0x879fdb55
	s[13] += s[7] ^ 0x25d75506
	s[20] += s[20] ^ 0xc40eceb7
	s[27] += s[33] ^ 0x62464868
	s[34] += s[46] ^ 0x7dc219
	s[41] += s[59] ^ 0x9eb53bca
	s[48] += s[8] ^ 0x3cecb57b
	s[55] += s[21] ^ 0xdb242f2c
	s[62] += s[34] ^ 0x795ba8dd
	s[5] += s[47] ^ 0x1793228e
	s[12] += s[60] ^ 0xb5ca9c3f
	s[19] += s[9] ^ 0x540215f0
	s[26] += s[22] ^ 0xf2398fa1
	s[33] += s[35] ^ 0x90710952
	s[40] += s[48] ^ 0x2ea88303
	s[47] += s[61] ^ 0xccdffcb4
	s[54] += s[10] ^ 0x6b177665
	s[61] += s[23] ^ 0x94ef016
	s[4] += s[36] ^ 0xa78669c7
	s[11] += s[49] ^ 0x45bde378
	s[18] += s[62] ^ 0xe3f55d29
	s[25] += s[11] ^ 0x822cd6da
	s[32] += s[24] ^ 0x2064508b
	s[39] += s[37] ^ 0xbe9bca3c
	s[46] += s[50] ^ 0x5cd343ed
	s[53] += s[63] ^ 0xfb0abd9e
	s[60] += s[12] ^ 0x9942374f
	s[3] += s[25] ^ 0x3779b100
	s[10] += s[38] ^ 0xd5b12ab1
	s[17] += s[51] ^ 0x73e8a462
	s[24] += s[0] ^ 0x12201e13
	s[31] += s[13] ^ 0xb05797c4
	s[38] += s[26] ^ 0x4e8f1175
	s[45] += s[39] ^ 0xecc68b26
	s[52] += s[52] ^ 0x8afe04d7
	s[59] += s[1] ^ 0x29357e88
	s[2] += s[14] ^ 0xc76cf839
	s[9] += s[27] ^ 0x65a471ea
	s[16] += s[40] ^ 0x3dbeb9b
	s[23] += s[53] ^ 0xa213654c
	s[30] += s[2] ^ 0x404adefd
	s[37] += s[15] ^ 0xde8258ae
	s[44] += s[28] ^ 0x7cb9d25f
	s[51] += s[41] ^ 0x1af14c10
	s[58] += s[54] ^ 0xb928c5c1
	s[1] += s[3] ^ 0x57603f72
	s[8] += s[16] ^ 0xf597b923
	s[15] += s[29] ^ 0x93cf32d4
	s[22] += s[42] ^ 0x3206ac85
	s[29] += s[55] ^ 0xd03e2636
	s[36] += s[4] ^ 0x6e759fe7
	s[43] += s[17] ^ 0xcad1998
	s[50] += s[30] ^ 0xaae49349
	s[57] += s[43] ^ 0x491c0cfa
	s[0] += s[56] ^ 0xe75386ab
	s[7] += s[5] ^ 0x858b005c
	s[14] += s[18] ^ 0x23c27a0d
	s[21] += s[31] ^ 0xc1f9f3be
	s[28] += s[44] ^ 0x60316d6f
	s[35] += s[57] ^ 0xfe68e720
	s[42] += s[6] ^ 0x9ca060d1
	s[49] += s[19] ^ 0x3ad7da82
	s[56] += s[32] ^ 0xd90f5433
	s[63] += s[45] ^ 0x7746cde4
	s[6] += s[58] ^ 0x157e4795
	s[13] += s[7] ^ 0xb3b5c146
	s[20] += s[20] ^ 0x51ed3af7
	s[27] += s[33] ^ 0xf024b4a8
	s[34] += s[46] ^ 0x8e5c2e59
	s[41] += s[59] ^ 0x2c93a80a
	s[48] += s[8] ^ 0xcacb21bb
	s[55] += s[21] ^ 0x69029b6c
	s[62] += s[34] ^ 0x73a151d
	s[5] += s[47] ^ 0xa5718ece
	s[12] += s[60] ^ 0x43a9087f
	s[19] += s[9] ^ 0xe1e08230
	s[26] += s[22] ^ 0x8017fbe1
	s[33] += s[35] ^ 0x1e4f7592
	s[40] += s[48] ^ 0xbc86ef43
	s[47] += s[61] ^ 0x5abe68f4
	s[54] += s[10] ^ 0xf8f5e2a5
	s[61] += s[23] ^ 0x972d5c56
	s[4] += s[36] ^ 0x3564d607
	s[11] += s[49] ^ 0xd39c4fb8
	s[18] += s[62] ^ 0x71d3c969
	s[25] += s[11] ^ 0x100b431a
	s[32] += s[24] ^ 0xae42bccb
	s[39] += s[37] ^ 0x4c7a367c
	s[46] += s[50] ^ 0xeab1b02d
	s[53] += s[63] ^ 0x88e929de
	s[60] += s[12] ^ 0x2720a38f
	s[3] += s[25] ^ 0xc5581d40
	s[10] += s[38] ^ 0x638f96f1
	s[17] += s[51] ^ 0x1c710a2
	s[24] += s[0] ^ 0x9ffe8a53
	s[31] += s[13] ^ 0x3e360404
	s[38] += s[26] ^ 0xdc6d7db5
	s[45] += s[39] ^ 0x7aa4f766
	s[52] += s[52] ^ 0x18dc7117
	s[59] += s[1] ^ 0xb713eac8
	s[2] += s[14] ^ 0x554b6479
	s[9] += s[27] ^ 0xf382de2a
	s[16] += s[40] ^ 0x91ba57db
	s[23] += s[53] ^ 0x2ff1d18c
	s[30] += s[2] ^ 0xce294b3d
	s[37] += s[15] ^ 0x6c60c4ee
	s[44] += s[28] ^ 0xa983e9f
	s[51] += s[41] ^ 0xa8cfb850
	s[58] += s[54] ^ 0x47073201
	s[1] += s[3] ^ 0xe53eabb2
	s[8] += s[16] ^ 0x83762563
	s[15] += s[29] ^ 0x21ad9f14
	s[22] += s[42] ^ 0xbfe518c5
	s[29] += s[55] ^ 0x5e1c9276
	s[36] += s[4] ^ 0xfc540c27
	s[43] += s[17] ^ 0x9a8b85d8
	s[50] += s[30] ^ 0x38c2ff89
	s[57] += s[43] ^ 0xd6fa793a
	s[0] += s[56] ^ 0x7531f2eb
	s[7] += s[5] ^ 0x13696c9c
	s[14] += s[18] ^ 0xb1a0e64d
	s[21] += s[31] ^ 0x4fd85ffe
	s[28] += s[44] ^ 0xee0fd9af
	s[35] += s[57] ^ 0x8c475360
	s[42] += s[6] ^ 0x2a7ecd11
	s[49] += s[19] ^ 0xc8b646c2
	s[56] += s[32] ^ 0x66edc073
	s[63] += s[45] ^ 0x5253a24
	s[6] += s[58] ^ 0xa35cb3d5
	s[13] += s[7] ^ 0x41942d86
	s[20] += s[20] ^ 0xdfcba737
	s[27] += s[33] ^ 0x7e0320e8
	s[34] += s[46] ^ 0x1c3a9a99
	s[41] += s[59] ^ 0xba72144a
	s[48] += s[8] ^ 0x58a98dfb
	s[55] += s[21] ^ 0xf6e107ac
	s[62] += s[34] ^ 0x9518815d
	s[5] += s[47] ^ 0x334ffb0e
	s[12] += s[60] ^ 0xd18774bf
	s[19] += s[9] ^ 0x6fbeee70
	s[26] += s[22] ^ 0xdf66821
	s[33] += s[35] ^ 0xac2de1d2
	s[40] += s[48] ^ 0x4a655b83
	s[47] += s[61] ^ 0xe89cd534
	s[54] += s[10] ^ 0x86d44ee5
	s[61] += s[23] ^ 0x250bc896
	s[4] += s[36] ^ 0xc3434247
	s[11] += s[49] ^ 0x617abbf8
	s[18] += s[62] ^ 0xffb235a9
	s[25] += s[11] ^ 0x9de9af5a
	s[32] += s[24] ^ 0x3c21290b
	s[39] += s[37] ^ 0xda58a2bc
	s[46] += s[50] ^ 0x78901c6d
	s[53] += s[63] ^ 0x16c7961e
	s[60] += s[12] ^ 0xb4ff0fcf
	s[3] += s[25] ^ 0x53368980
	s[10] += s[38] ^ 0xf16e0331
	s[17] += s[51] ^ 0x8fa57ce2
	s[24] += s[0] ^ 0x2ddcf693
	s[31] += s[13] ^ 0xcc147044
	s[38] += s[26] ^ 0x6a4be9f5
	s[45] += s[39] ^ 0x88363a6
	s[52] += s[52] ^ 0xa6badd57
	s[59] += s[1] ^ 0x44f25708
	s[2] += s[14] ^ 0xe329d0b9
	s[9] += s[27] ^ 0x81614a6a
	s[16] += s[40] ^ 0x1f98c41b
	s[23] += s[53] ^ 0xbdd03dcc
	s[30] += s[2] ^ 0x5c07b77d
	s[37] += s[15] ^ 0xfa3f312e
	s[44] += s[28] ^ 0x9876aadf
	s[51] += s[41] ^ 0x36ae2490
	s[58] += s[54] ^ 0xd4e59e41
	s[1] += s[3] ^ 0x731d17f2
	s[8] += s[16] ^ 0x115491a3
	s[15] += s[29] ^ 0xaf8c0b54
	s[22] += s[42] ^ 0x4dc38505
	s[29] += s[55] ^ 0xebfafeb6
	s[36] += s[4] ^ 0x8a327867
	s[43] += s[17] ^ 0x2869f218
	s[50] += s[30] ^ 0xc6a16bc9
	s[57] += s[43] ^ 0x64d8e57a
	s[0] += s[56] ^ 0x3105f2b
	s[7] += s[5] ^ 0xa147d8dc
	s[14] += s[18] ^ 0x3f7f528d
	s[21] += s[31] ^ 0xddb6cc3e
	s[28] += s[44] ^ 0x7bee45ef
	s[35] += s[57] ^ 0x1a25bfa0
	s[42] += s[6] ^ 0xb85d3951
	s[49] += s[19] ^ 0x5694b302
	s[56] += s[32] ^ 0xf4cc2cb3
	s[63] += s[45] ^ 0x9303a664
	s[6] += s[58] ^ 0x313b2015
	s[13] += s[7] ^ 0xcf7299c6
	s[20] += s[20] ^ 0x6daa1377
	s[27] += s[33] ^ 0xbe18d28
	s[34] += s[46] ^ 0xaa1906d9
	s[41] += s[59] ^ 0x4850808a
	s[48] += s[8] ^ 0xe687fa3b
	s[55] += s[21] ^ 0x84bf73ec
	s[62] += s[34] ^ 0x22f6ed9d
	s[5] += s[47] ^ 0xc12e674e
	s[12] += s[60] ^ 0x5f65e0ff
	s[19] += s[9] ^ 0xfd9d5ab0
	s[26] += s[22] ^ 0x9bd4d461
	s[33] += s[35] ^ 0x3a0c4e12
	s[40] += s[48] ^ 0xd843c7c3
	s[47] += s[61] ^ 0x767b4174
	s[54] += s[10] ^ 0x14b2bb25
	s[61] += s[23] ^ 0xb2ea34d6
	s[4] += s[36] ^ 0x5121ae87
	s[11] += s[49] ^ 0xef592838
	s[18] += s[62] ^ 0x8d90a1e9
	s[25] += s[11] ^ 0x2bc81b9a
	s[32] += s[24] ^ 0xc9ff954b
	s[39] += s[37] ^ 0x68370efc
	s[46] += s[50] ^ 0x66e88ad
	s[53] += s[63] ^ 0xa4a6025e
	s[60] += s[12] ^ 0x42dd7c0f
	s[3] += s[25] ^ 0xe114f5c0
	s[10] += s[38] ^ 0x7f4c6f71
	s[17] += s[51] ^ 0x1d83e922
	s[24] += s[0] ^ 0xbbbb62d3
	s[31] += s[13] ^ 0x59f2dc84
	s[38] += s[26] ^ 0xf82a5635
	s[45] += s[39] ^ 0x9661cfe6
	s[52] += s[52] ^ 0x34994997
	s[59] += s[1] ^ 0xd2d0c348
	s[2] += s[14] ^ 0x71083cf9
	s[9] += s[27] ^ 0xf3fb6aa
	s[16] += s[40] ^ 0xad77305b
	s[23] += s[53] ^ 0x4baeaa0c
	s[30] += s[2] ^ 0xe9e623bd
	s[37] += s[15] ^ 0x881d9d6e
	s[44] += s[28] ^ 0x2655171f
	s[51] += s[41] ^ 0xc48c90d0
	s[58] += s[54] ^ 0x62c40a81
	s[1] += s[3] ^ 0xfb8432
	s[8] += s[16] ^ 0x9f32fde3
	s[15] += s[29] ^ 0x3d6a7794
	s[22] += s[42] ^ 0xdba1f145
	s[29] += s[55] ^ 0x79d96af6
	s[36] += s[4] ^ 0x1810e4a7
	s[43] += s[17] ^ 0xb6485e58
	s[50] += s[30] ^ 0x547fd809
	s[57] += s[43] ^ 0xf2b751ba
	s[0] += s[56] ^ 0x90eecb6b
	s[7] += s[5] ^ 0x2f26451c
	s[14] += s[18] ^ 0xcd5dbecd
	s[21] += s[31] ^ 0x6b95387e
	s[28] += s[44] ^ 0x9ccb22f
	s[35] += s[57] ^ 0xa8042be0
	s[42] += s[6] ^ 0x463ba591
	s[49] += s[19] ^ 0xe4731f42
	s[56] += s[32] ^ 0x82aa98f3
	s[63] += s[45] ^ 0x20e212a4
	s[6] += s[58] ^ 0xbf198c55
	s[13] += s[7] ^ 0x5d510606
	s[20] += s[20] ^ 0xfb887fb7
	s[27] += s[33] ^ 0x99bff968
	s[34] += s[46] ^ 0x37f77319
	s[41] += s[59] ^ 0xd62eecca
	s[48] += s[8] ^ 0x7466667b
	s[55] += s[21] ^ 0x129de02c
	s[62] += s[34] ^ 0xb0d559dd
	s[5] += s[47] ^ 0x4f0cd38e
	s[12] += s[60] ^ 0xed444d3f
	s[19] += s[9] ^ 0x8b7bc6f0
	s[26] += s[22] ^ 0x29b340a1
	s[33] += s[35] ^ 0xc7eaba52
	s[40] += s[48] ^ 0x66223403
	s[47] += s[61] ^ 0x459adb4
	s[54] += s[10] ^ 0xa2912765
	s[61] += s[23] ^ 0x40c8a116
	s[4] += s[36] ^ 0xdf001ac7
	s[11] += s[49] ^ 0x7d379478
	s[18] += s[62] ^ 0x1b6f0e29
	s[25] += s[11] ^ 0xb9a687da
	s[32] += s[24] ^ 0x57de018b
	s[39] += s[37] ^ 0xf6157b3c
	s[46] += s[50] ^ 0x944cf4ed
	s[53] += s[63] ^ 0x32846e9e
	s[60] += s[12] ^ 0xd0bbe84f
	s[3] += s[25] ^ 0x6ef36200
	s[10] += s[38] ^ 0xd2adbb1
	s[17] += s[51] ^ 0xab625562
	s[24] += s[0] ^ 0x4999cf13
	s[31] += s[13] ^ 0xe7d148c4
	s[38] += s[26] ^ 0x8608c275
	s[45] += s[39] ^ 0x24403c26
	s[52] += s[52] ^ 0xc277b5d7
	s[59] += s[1] ^ 0x60af2f88
	s[2] += s[14] ^ 0xfee6a939
	s[9] += s[27] ^ 0x9d1e22ea
	s[16] += s[40] ^ 0x3b559c9b
	s[23] += s[53] ^ 0xd98d164c
	s[30] += s[2] ^ 0x77c48ffd
	s[37] += s[15] ^ 0x15fc09ae
	s[44] += s[28] ^ 0xb433835f
	s[51] += s[41] ^ 0x526afd10
	s[58] += s[54] ^ 0xf0a276c1
	s[1] += s[3] ^ 0x8ed9f072
	s[8] += s[16] ^ 0x2d116a23
	s[15] += s[29] ^ 0xcb48e3d4
	s[22] += s[42] ^ 0x69805d85
	s[29] += s[55] ^ 0x7b7d736
	s[36] += s[4] ^ 0xa5ef50e7
	s[43] += s[17] ^ 0x4426ca98
	s[50] += s[30] ^ 0xe25e4449
	s[57] += s[43] ^ 0x8095bdfa
	s[0] += s[56] ^ 0x1ecd37ab
	s[7] += s[5] ^ 0xbd04b15c
	s[14] += s[18] ^ 0x5b3c2b0d
	s[21] += s[31] ^ 0xf973a4be
	s[28] += s[44] ^ 0x97ab1e6f
	s[35] += s[57] ^ 0x35e29820
	s[42] += s[6] ^ 0xd41a11d1
	s[49] += s[19] ^ 0x72518b82
	s[56] += s[32] ^ 0x10890533
	s[63] += s[45] ^ 0xaec07ee4
	s[6] += s[58] ^ 0x4cf7f895
	s[13] += s[7] ^ 0xeb2f7246
	s[20] += s[20] ^ 0x8966ebf7
	s[27] += s[33] ^ 0x279e65a8
	s[34] += s[46] ^ 0xc5d5df59
	s[41] += s[59] ^ 0x640d590a
	s[48] += s[8] ^ 0x244d2bb
	s[55] += s[21] ^ 0xa07c4c6c
	s[62] += s[34] ^ 0x3eb3c61d
	s[5] += s[47] ^ 0xdceb3fce
	s[12] += s[60] ^ 0x7b22b97f
	s[19] += s[9] ^ 0x195a3330
	s[26] += s[22] ^ 0xb791ace1
	s[33] += s[35] ^ 0x55c92692
	s[40] += s[48] ^ 0xf400a043
	s[47] += s[61] ^ 0x923819f4
	s[54] += s[10] ^ 0x306f93a5
	s[61] += s[23] ^ 0xcea70d56
	s[4] += s[36] ^ 0x6cde8707
	s[11] += s[49] ^ 0xb1600b8
	s[18] += s[62] ^ 0xa94d7a69
	s[25] += s[11] ^ 0x4784f41a
	s[32] += s[24] ^ 0xe5bc6dcb
	s[39] += s[37] ^ 0x83f3e77c
	s[46] += s[50] ^ 0x222b612d
	s[53] += s[63] ^ 0xc062dade
	s[60] += s[12] ^ 0x5e9a548f
	s[3] += s[25] ^ 0xfcd1ce40
	s[10] += s[38] ^ 0x9b0947f1
	s[17] += s[51] ^ 0x3940c1a2
	s[24] += s[0] ^ 0xd7783b53
	s[31] += s[13] ^ 0x75afb504
	s[38] += s[26] ^ 0x13e72eb5
	s[45] += s[39] ^ 0xb21ea866
	s[52] += s[52] ^ 0x50562217
	s[59] += s[1] ^ 0xee8d9bc8
	s[2] += s[14] ^ 0x8cc51579
	s[9] += s[27] ^ 0x2afc8f2a
	s[16] += s[40] ^ 0xc93408db
	s[23] += s[53] ^ 0x676b828c
	s[30] += s[2] ^ 0x5a2fc3d
	s[37] += s[15] ^ 0xa3da75ee
	s[44] += s[28] ^ 0x4211ef9f
	s[51] += s[41] ^ 0xe0496950
	s[58] += s[54] ^ 0x7e80e301
	s[1] += s[3] ^ 0x1cb85cb2
	s[8] += s[16] ^ 0xbaefd663
	s[15] += s[29] ^ 0x59275014
	s[22] += s[42] ^ 0xf75ec9c5
	s[29] += s[55] ^ 0x95964376
	s[36] += s[4] ^ 0x33cdbd27
	s[43] += s[17] ^ 0xd20536d8
	s[50] += s[30] ^ 0x703cb089
	s[57] += s[43] ^ 0xe742a3a
	s[0] += s[56] ^ 0xacaba3eb
	s[7] += s[5] ^ 0x4ae31d9c
	s[14] += s[18] ^ 0xe91a974d
	s[21] += s[31] ^ 0x875210fe
	s[28] += s[44] ^ 0x25898aaf
	s[35] += s[57] ^ 0xc3c10460
	s[42] += s[6] ^ 0x61f87e11
	s[49] += s[19] ^ 0x2ff7c2
	s[56] += s[32] ^ 0x9e677173
	s[63] += s[45] ^ 0x3c9eeb24
	s[6] += s[58] ^ 0xdad664d5
	s[13] += s[7] ^ 0x790dde86
	s[20] += s[20] ^ 0x17455837
	s[27] += s[33] ^ 0xb57cd1e8
	s[34] += s[46] ^ 0x53b44b99
	s[41] += s[59] ^ 0xf1ebc54a
	s[48] += s[8] ^ 0x90233efb
	s[55] += s[21] ^ 0x2e5ab8ac
	s[62] += s[34] ^ 0xcc92325d
	s[5] += s[47] ^ 0x6ac9ac0e
	s[12] += s[60] ^ 0x90125bf
	s[19] += s[9] ^ 0xa7389f70
	s[26] += s[22] ^ 0x45701921
	s[33] += s[35] ^ 0xe3a792d2
	s[40] += s[48] ^ 0x81df0c83
	s[47] += s[61] ^ 0x20168634
	s[54] += s[10] ^ 0xbe4dffe5
	s[61] += s[23] ^ 0x5c857996
	s[4] += s[36] ^ 0xfabcf347
	s[11] += s[49] ^ 0x98f46cf8
	s[18] += s[62] ^ 0x372be6a9
	s[25] += s[11] ^ 0xd563605a
	s[32] += s[24] ^ 0x739ada0b
	s[39] += s[37] ^ 0x11d253bc
	s[46] += s[50] ^ 0xb009cd6d
	s[53] += s[63] ^ 0x4e41471e
	s[60] += s[12] ^ 0xec78c0cf
}

func TestSketchRewriteStartsWithoutConstructingAWholePayload(t *testing.T) {
	// Not parallel: it reads the process's heap statistics, which a test
	// running beside it would move.
	e := newTestEngine(t, Options{})
	require.NoError(t, e.OpenAOF(filepath.Join(t.TempDir(), "store.aof")))
	t.Cleanup(func() { e.CancelRewrite(); require.NoError(t, e.CloseAOF()); e.resetStores() })
	cms := data_structure.CreateCMS(1<<20, 1)
	e.cmsStore.Put("large", cms)
	require.NoError(t, e.StartRewrite())
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	require.NoError(t, e.AdvanceRewrite())
	runtime.ReadMemStats(&after)
	t.Logf("payload=%d allocated=%d first_slice=%d", cms.MarshalSize()+9, after.TotalAlloc-before.TotalAlloc, e.rewrite.written)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(256<<10))
	require.LessOrEqual(t, e.rewrite.written, int64(rewriteRecordSlice))
	require.NotNil(t, e.rewrite.stream)
	require.Empty(t, e.rewrite.stream.payload)
	require.NotNil(t, e.rewrite.stream.sketch)
}

func TestOpaqueRewriteReconcilesMutationWithValidHistoricalPayload(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	for _, kind := range []string{"bloom", "cms", "morris", "hll", "cuckoo"} {
		for _, mutation := range []string{"none", "update", "replace", "delete", "delete/slow-sync"} {
			t.Run(kind+"/"+mutation, func(t *testing.T) {
				e.resetStores()
				path := filepath.Join(t.TempDir(), "store.aof")
				require.NoError(t, e.OpenAOF(path))
				t.Cleanup(func() { e.CancelRewrite(); require.NoError(t, e.CloseAOF()); e.resetStores() })
				// A slow disk: the log's own fsync is still running when the
				// rewrite is ready to finish, as on a busy CI runner.
				slowSync := mutation == "delete/slow-sync"
				if slowSync {
					mutation = "delete"
					oldSync := e.aofSync
					e.aofSync = func(f *os.File) error { time.Sleep(300 * time.Millisecond); return f.Sync() }
					t.Cleanup(func() { e.aofSync = oldSync })
				}
				key := strings.Repeat("k", 96<<10)
				var update []string
				switch kind {
				case "bloom":
					runOn(t, e, "BF.RESERVE", key, "0.01", "100000")
					update = []string{"BF.ADD", key, "new"}
				case "cms":
					runOn(t, e, "CMS.INITBYDIM", key, "65536", "1")
					update = []string{"CMS.INCRBY", key, "new", "5"}
				case "morris":
					runOn(t, e, "MORRIS.INITBYDIM", key, "65536", "1")
					update = []string{"MORRIS.INCRBY", key, "new", "5"}
				case "hll":
					runOn(t, e, "PFADD", key, "old")
					update = []string{"PFADD", key, "new"}
				case "cuckoo":
					runOn(t, e, "CF.RESERVE", key, "65536")
					update = []string{"CF.ADD", key, "new"}
				}
				runOn(t, e, "PEXPIRE", key, "600000")
				require.NoError(t, e.FlushAOF())
				require.NoError(t, e.StartRewrite())
				require.NoError(t, e.AdvanceRewrite())
				require.NotNil(t, e.rewrite.stream)
				require.LessOrEqual(t, e.rewrite.written, int64(rewriteRecordSlice))
				switch mutation {
				case "update":
					runOn(t, e, update[0], update[1:]...)
				case "replace":
					runOn(t, e, "DEL", key)
					runOn(t, e, "SET", key, "replacement")
				case "delete":
					runOn(t, e, "DEL", key)
				}
				want, present := e.dumpKey(key)
				if slowSync {
					// Write the DEL and start its everysec sync now.
					e.aof.lastSync = time.Time{}
					require.NoError(t, e.flushAOF(false))
					require.NotNil(t, e.aof.syncPending, "the slow sync must be running while the rewrite advances")
				}
				for cycles := 0; e.RewriteActive(); cycles++ {
					waitForLogOn(e)
					waitForRewriteSyncOn(t, e)
					require.Less(t, cycles, 100)
					before := e.rewrite.written
					require.NoError(t, e.FlushAOF())
					require.LessOrEqual(t, e.rewrite.written-before, int64(rewriteRecordSlice))
				}
				require.NoError(t, e.CloseAOF())
				for restart := 0; restart < 2; restart++ {
					e.resetStores()
					_, err := e.LoadAOF(path)
					require.NoError(t, err, "every intermediate RESTORE must remain checksum-valid")
					got, exists := e.dumpKey(key)
					require.Equal(t, present, exists)
					require.Equal(t, want, got)
					if mutation == "none" || mutation == "update" {
						require.Positive(t, runOn(t, e, "PTTL", key).(int64))
					}
				}
			})
		}
	}
}
