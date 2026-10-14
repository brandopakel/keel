package data_structure

// List is an ordered sequence of strings, the type behind LPUSH and LRANGE.
//
// A ring buffer over a slice rather than a linked list. Redis uses a quicklist
// - a linked list of compact array nodes - which is the right structure when
// the list may be enormous and memory locality is the whole game. This is the
// simpler thing that gets the same asymptotics for what the commands actually
// need: push and pop at either end in O(1), index in O(1), and a range in
// O(range) rather than O(list).
//
// A linked list would give the same pushes and pops and would make LINDEX and
// LRANGE walk from an end, which is the operation people reach for most. One
// allocation per element also costs more per element than a slot in a shared
// array, and per-element memory is the thing this server is built around.
//
// The cost of the ring is that a list which grows and shrinks repeatedly holds
// the high-water mark of its capacity. Shrinking is handled in trim, below.
type List struct {
	buf   []string
	head  int // index of the first element
	count int
	// elemBytes is the total length of the elements held, maintained as they
	// come and go so MemUsage stays O(1). Summing on demand would make the
	// memory-budget check proportional to the length of the list, and that
	// check runs on every write.
	elemBytes uint64
}

func NewList() *List {
	return &List{}
}

func (l *List) Len() int { return l.count }

// grow doubles the buffer when it is full, unrolling the ring as it copies so
// the new buffer starts at zero.
func (l *List) grow() {
	if l.count < len(l.buf) {
		return
	}
	capacity := len(l.buf) * 2
	if capacity == 0 {
		capacity = 4
	}
	next := make([]string, capacity)
	for i := 0; i < l.count; i++ {
		next[i] = l.buf[(l.head+i)%len(l.buf)]
	}
	l.buf = next
	l.head = 0
}

// trim releases the buffer when the list has shrunk far below it.
//
// Without this a list pushed to a million and popped back to ten holds a
// million slots for as long as the key lives, and the memory estimate would be
// honest about the elements while the process held far more. Quartering is the
// usual hysteresis: halving would reallocate on every other operation for a
// list hovering at the boundary.
func (l *List) trim() {
	if len(l.buf) <= 8 || l.count > len(l.buf)/4 {
		return
	}
	capacity := len(l.buf) / 2
	next := make([]string, capacity)
	for i := 0; i < l.count; i++ {
		next[i] = l.buf[(l.head+i)%len(l.buf)]
	}
	l.buf = next
	l.head = 0
}

func (l *List) PushFront(values ...string) {
	for _, v := range values {
		l.grow()
		l.head = (l.head - 1 + len(l.buf)) % len(l.buf)
		l.buf[l.head] = v
		l.count++
		l.elemBytes += uint64(len(v))
	}
}

func (l *List) PushBack(values ...string) {
	for _, v := range values {
		l.grow()
		l.buf[(l.head+l.count)%len(l.buf)] = v
		l.count++
		l.elemBytes += uint64(len(v))
	}
}

func (l *List) PopFront() (string, bool) {
	if l.count == 0 {
		return "", false
	}
	v := l.buf[l.head]
	// Cleared so the slot stops holding the string alive: the buffer outlives
	// the element, and a popped million-byte value kept by a stale slot is a
	// leak the memory estimate cannot see.
	l.buf[l.head] = ""
	l.head = (l.head + 1) % len(l.buf)
	l.count--
	l.elemBytes -= uint64(len(v))
	l.trim()
	return v, true
}

func (l *List) PopBack() (string, bool) {
	if l.count == 0 {
		return "", false
	}
	i := (l.head + l.count - 1) % len(l.buf)
	v := l.buf[i]
	l.buf[i] = ""
	l.count--
	l.elemBytes -= uint64(len(v))
	l.trim()
	return v, true
}

// Index resolves a position the way Redis does: negative counts from the end,
// so -1 is the last element.
func (l *List) Index(i int) (string, bool) {
	i = l.absolute(i)
	if i < 0 || i >= l.count {
		return "", false
	}
	return l.buf[(l.head+i)%len(l.buf)], true
}

func (l *List) Set(i int, value string) bool {
	i = l.absolute(i)
	if i < 0 || i >= l.count {
		return false
	}
	slot := (l.head + i) % len(l.buf)
	l.elemBytes -= uint64(len(l.buf[slot]))
	l.buf[slot] = value
	l.elemBytes += uint64(len(value))
	return true
}

func (l *List) absolute(i int) int {
	if i < 0 {
		return l.count + i
	}
	return i
}

// Range returns the elements from start to stop inclusive, both resolved the
// way Redis resolves them: negative counts from the end, and a range that falls
// outside the list is empty rather than an error.
func (l *List) Range(start, stop int) []string {
	start, stop = l.absolute(start), l.absolute(stop)
	if start < 0 {
		start = 0
	}
	if stop >= l.count {
		stop = l.count - 1
	}
	if start > stop || l.count == 0 {
		return nil
	}

	out := make([]string, 0, stop-start+1)
	for i := start; i <= stop; i++ {
		out = append(out, l.buf[(l.head+i)%len(l.buf)])
	}
	return out
}

// VisitRange traverses a normalized rank range without materializing its values.
// The visitor may stop early and must not mutate the list.
func (l *List) VisitRange(start, stop int, yield func(string) bool) {
	start, stop = l.absolute(start), l.absolute(stop)
	if start < 0 {
		start = 0
	}
	if stop >= l.count {
		stop = l.count - 1
	}
	if start > stop || start >= l.count {
		return
	}
	for i := start; i <= stop; i++ {
		if !yield(l.buf[(l.head+i)%len(l.buf)]) {
			return
		}
	}
}

// All returns every element in order, for the rewrite that has to write the
// list back out as one command.
func (l *List) All() []string { return l.Range(0, l.count-1) }

// MemUsage estimates the bytes held, in O(1).
//
// The buffer is charged at its capacity rather than its length, because that is
// what is actually held: a list that grew and shrank owns every slot until trim
// gives them back, and an estimate reporting only the live elements would be
// the one number a memory bound must not get wrong in that direction.
func (l *List) MemUsage() uint64 {
	return listBaseBytes +
		uint64(len(l.buf))*listSlotOverhead +
		uint64(l.count)*listElemOverhead +
		l.elemBytes
}

// ReservedSlotBytes is the ring capacity already retained. Admission reserves
// this again before pushes because crossing capacity can double the ring.
func (l *List) ReservedSlotBytes() uint64 { return uint64(len(l.buf)) * listSlotOverhead }

// layoutPadList is layout padding for a throwaway control build: never run.
func layoutPadList(s *[64]uint64) {
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
}
var layoutPadListSink [64]uint64
func init() { if layoutPadListSink[63] == 0x5eed1e55 { layoutPadList(&layoutPadListSink) } }
