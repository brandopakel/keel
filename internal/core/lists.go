package core

import (
	"errors"
	"strconv"

	"github.com/brandopakel/keel/internal/data_structure"
)

// Typed operations: lists (phase 4 of docs/embedding-plan.md, converted as
// docs/phase4-family-recipe.md converts a family).
//
// Each operation runs in a command's scope, after the type check of the key
// it names, and stages the record of every write it makes, so the list
// commands are never logged as they arrived (see strings.go). What the
// as-arrived log held is what they stage, byte for byte: a client's integers
// are canonical (canonicalInteger), so an index or a count written back in
// decimal is the argument the client sent.
//
// Where Redis answers a missing key before it reads the arguments after the
// key, or must build its reply before it writes, the lookup (listFor) is the
// command language's, split from the operation as SET's lookup is split from
// its write: LPOP and RPOP admit the elements' reply before they remove them,
// so a reply that cannot be sent leaves the list as it was.
//
// A list has no empty form: whatever empties one deletes the key
// (dropListIfEmpty).

var (
	// errNoSuchKey is LSET on a key that holds nothing, in Redis's words.
	errNoSuchKey = errors.New("ERR no such key")
	// errIndexOutOfRange is LSET past either end of the list.
	errIndexOutOfRange = errors.New("ERR index out of range")
)

// listFor is the list key holds, and false when it holds none. A key whose
// expiry has passed is reaped, and holds none.
func (e *Engine) listFor(key string) (*data_structure.List, bool) {
	return e.listStore.Get(key)
}

// dropListIfEmpty deletes the key of a list a write emptied, and otherwise
// resizes the store's account of it. A key with no elements would answer
// EXISTS 1 and LRANGE nothing, and the rewrite would write it as an RPUSH with
// no values, which is a syntax error on replay.
func (e *Engine) dropListIfEmpty(key string, l *data_structure.List) {
	if l.Len() == 0 {
		e.listStore.Delete(key)
		return
	}
	e.listStore.Resize(key)
}

// opPush adds values at the head of the list key holds (front), each in turn,
// so that LPUSH k a b leaves b first, or at its tail, creating the list if
// key holds none, and answers the list's length after it: LPUSH and RPUSH.
func (e *Engine) opPush(key string, values []string, front bool) int64 {
	l, ok := e.listFor(key)
	if !ok {
		l = data_structure.NewList()
		e.listStore.Put(key, l)
	}
	name := "RPUSH"
	if front {
		name = "LPUSH"
		l.PushFront(values...)
	} else {
		l.PushBack(values...)
	}
	e.listStore.Resize(key)
	e.aofRecordKeyArgs(name, key, values)
	return int64(l.Len())
}

// listPop is what an LPOP or RPOP asks for: the end it pops from, and with
// a count, how many elements.
type listPop struct {
	front   bool
	counted bool
	count   int64
}

// name is the command the pop is recorded as.
func (p listPop) name() string {
	if p.front {
		return "LPOP"
	}
	return "RPOP"
}

// popped is how many elements the pop takes from a list of n: one, or its
// count, at most the whole list.
func (p listPop) popped(n int) int {
	if !p.counted {
		return min(1, n)
	}
	return int(min(p.count, int64(n)))
}

// opPop removes what req asks for from l, the list key holds, once the
// command language has answered with those elements, and deletes the key if
// that empties it.
//
// A pop is recorded as it was asked for, its count included, as the log has
// always held it - also when l is nil because key holds no list, and when
// the count is zero, which change nothing. Redis propagates neither
// (t_list.c, popGenericCommand: no server.dirty for a missing key or a zero
// count); the records replay as the no-ops they are.
func (e *Engine) opPop(key string, l *data_structure.List, req listPop) {
	if l != nil {
		for i := req.popped(l.Len()); i > 0; i-- {
			if req.front {
				l.PopFront()
			} else {
				l.PopBack()
			}
		}
		e.dropListIfEmpty(key, l)
	}
	if req.counted {
		e.aofRecordInt(req.name(), key, req.count)
	} else {
		e.aofRecord(req.name(), key)
	}
}

// opLLen is the length of the list key holds, zero when it holds none: LLEN.
func (e *Engine) opLLen(key string) int64 {
	if l, ok := e.listFor(key); ok {
		return int64(l.Len())
	}
	return 0
}

// opLIndex is the element at index of the list key holds, counting from the
// tail when index is negative, and false when key holds no list or the index
// is past either end: LINDEX.
func (e *Engine) opLIndex(key string, index int64) (string, bool) {
	l, ok := e.listFor(key)
	if !ok {
		return "", false
	}
	return l.Index(int(index))
}

// listRange is the elements of a list from start to stop, resolved as LRANGE
// resolves them, and read where they are rather than copied.
type listRange struct {
	l           *data_structure.List
	start, stop int
}

// visit yields the range's elements in order, until yield returns false.
func (r listRange) visit(yield func(string) bool) { r.l.VisitRange(r.start, r.stop, yield) }

// opLRange is the elements of the list key holds from start to stop, both
// inclusive, counting from the tail when negative, and empty when the range
// falls outside the list; false when key holds no list: LRANGE. Its lookup
// is a keyspace hit or a miss, counted here, after the command language has
// read start and stop, as Redis's lrangeCommand reads them before it looks
// (readsAfterArguments).
func (e *Engine) opLRange(key string, start, stop int) (listRange, bool) {
	l, ok := e.listFor(key)
	e.noteLookup(ok)
	return listRange{l: l, start: start, stop: stop}, ok
}

// opLSet replaces the element at index of the list key holds, counting from
// the tail when index is negative: LSET. A key that holds no list is
// errNoSuchKey, and an index past either end errIndexOutOfRange; neither
// changes anything.
func (e *Engine) opLSet(key string, index int64, value string) error {
	l, ok := e.listFor(key)
	if !ok {
		return errNoSuchKey
	}
	if !l.Set(int(index), value) {
		return errIndexOutOfRange
	}
	e.listStore.Resize(key)
	e.aofRecord("LSET", key, strconv.FormatInt(index, 10), value)
	return nil
}

// opLTrim keeps the elements of the list key holds from start to stop, both
// inclusive and resolved as LRANGE resolves them, deleting the key when
// nothing is left: LTRIM. The list keeps its expiry.
//
// A trim is recorded as it was asked for, also when key holds no list, as
// the log has always held it. Redis propagates nothing then (t_list.c,
// ltrimCommand: a missing key returns before server.dirty); the record
// replays as the no-op it is.
func (e *Engine) opLTrim(key string, start, stop int) {
	if l, ok := e.listFor(key); ok {
		values := l.Range(start, stop)
		if len(values) == 0 {
			e.listStore.Delete(key)
		} else {
			ttl, expires := e.listStore.GetExpiry(key)
			replacement := data_structure.NewList()
			replacement.PushBack(values...)
			e.listStore.Put(key, replacement)
			if expires {
				e.listStore.SetExpiryAt(key, ttl)
			}
		}
	}
	e.aofRecord("LTRIM", key, strconv.Itoa(start), strconv.Itoa(stop))
}

// layoutPadLists is layout padding for a throwaway control build: never run.
func layoutPadLists(s *[64]uint64) {
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
}
