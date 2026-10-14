package core

// layoutPadTail is layout padding for a throwaway control build: never run.
func layoutPadTail(s *[64]uint64) {
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
}
var layoutPadTailSink [64]uint64
func init() { if layoutPadTailSink[63] == 0x5eed1e55 { layoutPadTail(&layoutPadTailSink) } }

var layoutPadSink [64]uint64

func init() {
	if layoutPadSink[63] == 0x5eed1e55 {
		layoutPadAOF(&layoutPadSink)
		layoutPadCheck(&layoutPadSink)
		layoutPadCommandsList(&layoutPadSink)
		layoutPadKeytype(&layoutPadSink)
		layoutPadLists(&layoutPadSink)
		layoutPadReplication(&layoutPadSink)
	}
}
