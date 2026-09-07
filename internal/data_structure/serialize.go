package data_structure

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Serialising the structures that no command can rebuild.
//
// Rewriting the append-only file means writing the shortest log that produces
// the current state. For most types that is a command: a string is one SET, a
// set is one SADD of its members, a sorted set one ZADD. The state is small and
// the commands to recreate it are obvious.
//
// The probabilistic types are not like that. A HyperLogLog is 12KB of registers
// arrived at by hashing every item ever added, and nothing short of those items
// reproduces it - but the items were never stored, which is the entire point of
// the structure. The same holds for the Bloom and cuckoo filters and for both
// sketches. Their state is bounded and their history is not, so a rewrite that
// could not write the state would have to keep the history, and the file would
// go on growing for exactly the keys where it grows fastest.
//
// So these five are written as bytes. The format is deliberately plain -
// little-endian fields in declaration order, no version tag, no checksum -
// because it is read only by the process that wrote it, on the machine that
// wrote it, from a file it also wrote. A format meant to travel between
// machines or versions would need both, and would be a different feature.

var errShortPayload = errors.New("serialised payload ends early")

// buf is a tiny append-only writer, and cursor its counterpart for reading.
// Both exist so the marshallers below read as a list of fields rather than as
// slice arithmetic.
type buf struct{ b []byte }

func (w *buf) u32(v uint32)  { w.b = binary.LittleEndian.AppendUint32(w.b, v) }
func (w *buf) u64(v uint64)  { w.b = binary.LittleEndian.AppendUint64(w.b, v) }
func (w *buf) f64(v float64) { w.u64(math.Float64bits(v)) }
func (w *buf) bytes(p []byte) {
	w.u64(uint64(len(p)))
	w.b = append(w.b, p...)
}

type cursor struct {
	b   []byte
	err error
}

func (r *cursor) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || len(r.b) < n {
		r.err = errShortPayload
		return nil
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out
}

func (r *cursor) u32() uint32 {
	p := r.take(4)
	if p == nil {
		return 0
	}
	return binary.LittleEndian.Uint32(p)
}

func (r *cursor) u64() uint64 {
	p := r.take(8)
	if p == nil {
		return 0
	}
	return binary.LittleEndian.Uint64(p)
}

func (r *cursor) f64() float64 { return math.Float64frombits(r.u64()) }

func (r *cursor) bytes() []byte {
	n := r.u64()
	if r.err != nil {
		return nil
	}
	// Length-prefixed, and the length comes from the file, so it is checked
	// against what is actually there before it becomes an allocation.
	if uint64(len(r.b)) < n || n > MaxStructureBytes {
		r.err = errShortPayload
		return nil
	}
	out := make([]byte, n)
	copy(out, r.take(int(n)))
	return out
}

// --- Count-Min sketch ---

func (c *CMS) Marshal() []byte {
	w := &buf{}
	w.u32(c.width)
	w.u32(c.depth)
	w.u64(c.totalCount)
	for _, v := range c.counter {
		w.u32(v)
	}
	return w.b
}

func UnmarshalCMS(p []byte) (*CMS, error) {
	r := &cursor{b: p}
	c := &CMS{width: r.u32(), depth: r.u32(), totalCount: r.u64()}
	if r.err != nil {
		return nil, r.err
	}
	if err := checkTable(uint64(c.width), uint64(c.depth), len(r.b), 4); err != nil {
		return nil, fmt.Errorf("count-min sketch: %w", err)
	}
	c.counter = make([]uint32, uint64(c.width)*uint64(c.depth))
	for i := range c.counter {
		c.counter[i] = r.u32()
	}
	return c, r.err
}

// --- Morris counter ---

func (m *Morris) Marshal() []byte {
	w := &buf{}
	w.u32(m.width)
	w.u32(m.depth)
	w.u64(m.totalCount)
	w.u64(m.rngState)
	w.b = append(w.b, m.counters...)
	return w.b
}

func UnmarshalMorris(p []byte) (*Morris, error) {
	r := &cursor{b: p}
	m := &Morris{width: r.u32(), depth: r.u32(), totalCount: r.u64(), rngState: r.u64()}
	if r.err != nil {
		return nil, r.err
	}
	if err := checkTable(uint64(m.width), uint64(m.depth), len(r.b), 1); err != nil {
		return nil, fmt.Errorf("morris counter: %w", err)
	}
	if m.rngState == 0 {
		return nil, errors.New("morris counter: invalid RNG")
	}
	n := uint64(m.width) * uint64(m.depth)
	m.counters = make([]uint8, n)
	copy(m.counters, r.take(int(n)))
	return m, r.err
}

// checkTable rejects dimensions that do not match the bytes that follow, before
// they turn into an allocation. A width and depth read out of a file are not
// facts, and multiplied together they are an easy way to ask for a terabyte.
func checkTable(w, d uint64, remaining int, cellBytes uint64) error {
	if w == 0 || d == 0 {
		return errors.New("zero dimension")
	}
	if w > math.MaxUint32/d {
		return errors.New("dimensions overflow")
	}
	if want := w * d * cellBytes; want > MaxStructureBytes || want != uint64(remaining) {
		return fmt.Errorf("%dx%d needs %d bytes, payload has %d", w, d, want, remaining)
	}
	return nil
}

// --- HyperLogLog ---

func (h *HLL) Marshal() []byte {
	// Keep the alpha dense wire format byte-for-byte, without expanding the
	// live sketch or allocating an intermediate dense buffer.
	body := make([]byte, 8+hllDenseSize)
	binary.LittleEndian.PutUint64(body, hllDenseSize)
	if h.regs != nil {
		copy(body[8:], h.regs)
	} else {
		packed := HLL{regs: body[8:]}
		for _, entry := range h.sparse {
			packed.setRegister(int(entry>>hllBits), uint8(entry&hllRegisterMax))
		}
	}
	return body
}

func UnmarshalHLL(p []byte) (*HLL, error) {
	r := &cursor{b: p}
	regs := r.bytes()
	if r.err != nil {
		return nil, r.err
	}
	if want := (hllRegisters*hllBits)/8 + 1; len(regs) != want {
		return nil, fmt.Errorf("hyperloglog: register array is %d bytes, expected %d", len(regs), want)
	}
	// cachedCount is left invalid rather than stored: it is an optimisation
	// derived from the registers, and recomputing it costs one pass the first
	// time anyone asks.
	h := &HLL{regs: regs}
	if len(r.b) != 0 {
		return nil, errors.New("hyperloglog: trailing bytes")
	}
	nonzero := 0
	for i := 0; i < hllRegisters; i++ {
		if h.getRegister(i) > hllQ+1 {
			return nil, errors.New("hyperloglog: invalid register")
		}
		if h.getRegister(i) != 0 {
			nonzero++
		}
	}
	// Preserve any legacy spare-byte content. Canonical dumps have zero here.
	if nonzero <= hllSparseLimit && regs[len(regs)-1] == 0 {
		compact := CreateHLL()
		for i := 0; i < hllRegisters; i++ {
			if v := h.getRegister(i); v != 0 {
				compact.setRegister(i, v)
			}
		}
		h = compact
	}
	return h, nil
}

// --- Cuckoo filter ---

func (c *CuckooFilter) Marshal() []byte {
	w := &buf{}
	w.u64(c.numBuckets)
	w.u64(c.inserted)
	w.u64(c.deleted)
	w.u64(c.capacity)
	w.u64(c.rngState)
	w.u64(uint64(len(c.buckets)))
	for _, v := range c.buckets {
		w.b = binary.LittleEndian.AppendUint16(w.b, v)
	}
	return w.b
}

func UnmarshalCuckoo(p []byte) (*CuckooFilter, error) {
	r := &cursor{b: p}
	c := &CuckooFilter{
		numBuckets: r.u64(),
		inserted:   r.u64(),
		deleted:    r.u64(),
		capacity:   r.u64(),
		rngState:   r.u64(),
	}
	n := r.u64()
	if r.err != nil {
		return nil, r.err
	}
	// The table size must be a power of two or altIndex can produce an index
	// outside it, which would be a silent read of the wrong bucket rather than
	// a crash.
	if c.numBuckets == 0 || c.numBuckets&(c.numBuckets-1) != 0 {
		return nil, fmt.Errorf("cuckoo filter: %d buckets is not a power of two", c.numBuckets)
	}
	if c.numBuckets > MaxStructureBytes/(CuckooBucketSize*2) || n > MaxStructureBytes/2 || n != uint64(len(r.b))/2 || uint64(len(r.b))%2 != 0 || c.deleted > c.inserted || c.rngState == 0 {
		return nil, errors.New("cuckoo filter: invalid dimensions or state")
	}
	if n != c.numBuckets*CuckooBucketSize {
		return nil, fmt.Errorf("cuckoo filter: %d buckets needs %d slots, payload has %d",
			c.numBuckets, c.numBuckets*CuckooBucketSize, n)
	}
	c.mask = c.numBuckets - 1
	c.buckets = make([]uint16, n)
	for i := range c.buckets {
		q := r.take(2)
		if q == nil {
			return nil, r.err
		}
		c.buckets[i] = binary.LittleEndian.Uint16(q)
	}
	return c, r.err
}

// --- Scalable Bloom filter ---

func (s *SBChain) Marshal() []byte {
	w := &buf{}
	w.u64(s.size)
	w.u64(s.growthFactor)
	w.u64(uint64(len(s.filters)))
	for i := range s.filters {
		link := &s.filters[i]
		w.u64(link.size)
		w.u32(uint32(link.bloom.Hashes))
		w.u64(link.bloom.Entries)
		w.f64(link.bloom.Error)
		w.f64(link.bloom.bitPerEntry)
		w.u64(link.bloom.bits)
		w.bytes(link.bloom.bf)
	}
	return w.b
}

func UnmarshalSBChain(p []byte) (*SBChain, error) {
	r := &cursor{b: p}
	s := &SBChain{size: r.u64(), growthFactor: r.u64()}
	count := r.u64()
	if r.err != nil {
		return nil, r.err
	}
	// One link is a few dozen bytes at minimum, so a count larger than the
	// bytes remaining cannot be honest and must not become an allocation.
	if count == 0 || s.growthFactor == 0 || count > uint64(len(r.b))/53 {
		return nil, fmt.Errorf("bloom filter: %d links in %d bytes", count, len(r.b))
	}

	s.filters = make([]SBLink, 0, count)
	for i := uint64(0); i < count; i++ {
		link := SBLink{size: r.u64()}
		b := &Bloom{}
		b.Hashes = int(r.u32())
		b.Entries = r.u64()
		b.Error = r.f64()
		b.bitPerEntry = r.f64()
		b.bits = r.u64()
		b.bf = r.bytes()
		if r.err != nil {
			return nil, r.err
		}
		if b.Hashes < 1 || b.Hashes > 2048 || b.bits == 0 || b.Entries == 0 || math.IsNaN(b.Error) || b.Error <= 0 || b.Error >= 1 || math.IsNaN(b.bitPerEntry) || math.IsInf(b.bitPerEntry, 0) || b.bitPerEntry <= 0 {
			return nil, errors.New("bloom filter: invalid state")
		}
		if uint64(len(b.bf))*8 < b.bits {
			return nil, fmt.Errorf("bloom filter: %d bits do not fit in %d bytes", b.bits, len(b.bf))
		}
		b.bytes = uint64(len(b.bf))
		link.bloom = b
		s.filters = append(s.filters, link)
	}
	if len(r.b) != 0 {
		return nil, fmt.Errorf("bloom filter: %d bytes left over", len(r.b))
	}
	return s, nil
}

// --- Sorted set ---

// Entries returns every member and its score, so a sorted set can be written
// out as the one ZADD that recreates it.
func (zs *ZSet) Entries() ([]string, []float64) {
	members := make([]string, 0, len(zs.dict))
	scores := make([]float64, 0, len(zs.dict))
	for member, score := range zs.dict {
		members = append(members, member)
		scores = append(scores, score)
	}
	return members, scores
}
