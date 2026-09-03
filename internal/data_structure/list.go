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
