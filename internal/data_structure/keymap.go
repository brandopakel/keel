package data_structure

import (
	"fmt"
	"hash/maphash"
	"math/bits"
	"time"
)

// Pages give each live key a stable slot. Growing the lookup map or page table
// does not move slots. A cursor is a slot number, so it resumes without retaining
// an iterator or copying the keyspace. Deleted slots are reused; a key present
// throughout an iteration keeps its position and is visited exactly once.
//
// Values are stored inline in pages. The lookup map holds slot numbers rather
// than pointers to separately allocated entries. Empty pages release their keys
// and values immediately; the small directory remains reusable until the store
// empties. There is no allocation or abandoned-state budget per SCAN cursor.
const keyPageSlots = 64
const scanStoreShift = 48
const scanSlotMask = (uint64(1) << scanStoreShift) - 1
const ScanMaxWork = 1024
const ScanByteTarget = 64 << 10
const scanTimeTarget = time.Millisecond

type keySlot[V any] struct {
	key   string
	value V
}
type keyPage[V any] struct {
	slots [keyPageSlots]keySlot[V]
	used  uint64
}
type keyPageRef[V any] struct {
	page     *keyPage[V]
	nextFree int
}

// Lookup identity is independent of traversal; collisions never merge keys.
var keyLookupSeed = maphash.MakeSeed()

type keyMap[V any] struct {
	collisions   map[uint64][]uint64
	hashOverride func(string) uint64 // collision injection in tests; nil in production
	lookup       map[uint64]uint64
	pages        []keyPageRef[V]
	free         int // page index plus one; zero means no page with available slots
	end          uint64
	count        int
}

func (m *keyMap[V]) position(key string) (uint64, uint64, bool) {
	h := uint64(0)
	if m.hashOverride != nil {
		h = m.hashOverride(key)
	} else {
		h = maphash.String(keyLookupSeed, key)
	}
	pos, ok := m.lookup[h]
	if ok {
		if m.pages[pos/keyPageSlots].page.slots[pos%keyPageSlots].key == key {
			return pos, h, true
		}
		for _, other := range m.collisions[h] {
			if m.pages[other/keyPageSlots].page.slots[other%keyPageSlots].key == key {
				return other, h, true
			}
		}
	}
	return 0, h, false
}
func (m *keyMap[V]) getPtr(key string) (*V, bool) {
	pos, _, ok := m.position(key)
	if !ok {
		return nil, false
	}
	return &m.pages[pos/keyPageSlots].page.slots[pos%keyPageSlots].value, true
}
func (m *keyMap[V]) get(key string) (V, bool) {
	p, ok := m.getPtr(key)
	if !ok {
		var zero V
		return zero, false
	}
	return *p, true
}
func (m *keyMap[V]) set(key string, value V) bool {
	existing, hash, exists := m.position(key)
	if exists {
		m.pages[existing/keyPageSlots].page.slots[existing%keyPageSlots].value = value
		return true
	}
	if m.lookup == nil {
		m.lookup = make(map[uint64]uint64)
	}
	if m.free == 0 {
		m.pages = append(m.pages, keyPageRef[V]{})
		m.free = len(m.pages)
	}
	index := m.free - 1
	ref := &m.pages[index]
	if ref.page == nil {
		ref.page = new(keyPage[V])
	}
	p := ref.page
	slot := bits.TrailingZeros64(^p.used)
	pos := uint64(index)*keyPageSlots + uint64(slot)
	p.slots[slot] = keySlot[V]{key: key, value: value}
	p.used |= uint64(1) << slot
	if p.used == ^uint64(0) {
		m.free = ref.nextFree
		ref.nextFree = 0
	}
	if _, exists := m.lookup[hash]; exists {
		if m.collisions == nil {
			m.collisions = make(map[uint64][]uint64)
		}
		m.collisions[hash] = append(m.collisions[hash], pos)
	} else {
		m.lookup[hash] = pos
	}
	m.end = max(m.end, pos+1)
	m.count++
	return false
}
func (m *keyMap[V]) del(key string) bool {
	pos, hash, ok := m.position(key)
	if !ok {
		return false
	}
	extra := m.collisions[hash]
	if m.lookup[hash] == pos {
		if len(extra) == 0 {
			delete(m.lookup, hash)
		} else {
			m.lookup[hash] = extra[len(extra)-1]
			extra = extra[:len(extra)-1]
		}
	} else {
		for i, p := range extra {
			if p == pos {
				extra[i] = extra[len(extra)-1]
				extra = extra[:len(extra)-1]
				break
			}
		}
	}
	if len(extra) == 0 {
		delete(m.collisions, hash)
	} else {
		m.collisions[hash] = extra
	}
	index := int(pos / keyPageSlots)
	ref := &m.pages[index]
	p := ref.page
	if p.used == ^uint64(0) {
		ref.nextFree = m.free
		m.free = index + 1
	}
	p.used &^= uint64(1) << (pos % keyPageSlots)
	var zero keySlot[V]
	p.slots[pos%keyPageSlots] = zero
	if p.used == 0 {
		ref.page = nil
	}
	m.count--
	if m.count == 0 {
		*m = keyMap[V]{hashOverride: m.hashOverride}
	}
	return true
}
func (m *keyMap[V]) len() int        { return m.count }
func (m *keyMap[V]) scanEnd() uint64 { return m.end }
func (m *keyMap[V]) keys() []string {
	keys := make([]string, 0, m.count)
	for _, ref := range m.pages {
		if ref.page == nil {
			continue
		}
		used := ref.page.used
		for used != 0 {
			i := bits.TrailingZeros64(used)
			used &^= uint64(1) << i
			keys = append(keys, ref.page.slots[i].key)
		}
	}
	return keys
}

// scan charges one unit per live slot examined or empty page skipped, including
// filtered-out keys. It never exceeds the requested work or ScanMaxWork. Byte
// and time targets are checked between names; one oversized name is processed
// alone to make progress. This is cooperative scheduling, not a real-time SLA.
func (m *keyMap[V]) scan(cursor uint64, budget int, keep func(string) bool, dst []string) ([]string, int, uint64) {
	return m.scanUntil(cursor, m.end, budget, keep, dst)
}
func (m *keyMap[V]) scanUntil(cursor, end uint64, budget int, keep func(string) bool, dst []string) ([]string, int, uint64) {
	end = min(end, m.end)
	if cursor >= end {
		return dst, 0, 0
	}
	budget = min(max(budget, 1), ScanMaxWork)
	deadline := time.Now().Add(scanTimeTarget)
	examined, bytes := 0, 0
	for cursor < end && examined < budget {
		p := m.pages[cursor/keyPageSlots].page
		if p == nil || p.used>>(cursor%keyPageSlots) == 0 {
			cursor = (cursor/keyPageSlots + 1) * keyPageSlots
			examined++
		} else {
			cursor += uint64(bits.TrailingZeros64(p.used >> (cursor % keyPageSlots)))
			if cursor >= end {
				break
			}
			key := p.slots[cursor%keyPageSlots].key
			if examined > 0 && bytes+len(key)+16 > ScanByteTarget {
				break
			}
			cursor++
			examined++
			bytes += len(key) + 16
			if keep == nil || keep(key) {
				dst = append(dst, key)
			}
		}
		if bytes >= ScanByteTarget || time.Now().After(deadline) {
			break
		}
	}
	if cursor >= end {
		cursor = 0
	}
	return dst, examined, cursor
}
func (m *keyMap[V]) sample(n int, visit func(string, V)) {
	for hash, pos := range m.lookup {
		if n <= 0 {
			return
		}
		entry := m.pages[pos/keyPageSlots].page.slots[pos%keyPageSlots]
		visit(entry.key, entry.value)
		n--
		for _, pos := range m.collisions[hash] {
			if n <= 0 {
				return
			}
			entry := m.pages[pos/keyPageSlots].page.slots[pos%keyPageSlots]
			visit(entry.key, entry.value)
			n--
		}
	}
}

// ScanKeyspaces packs a store index and stable slot into an opaque cursor.
// Empty stores consume work too. A caller that filters everything out still
// receives a resumable cursor without an unbounded traversal.
func ScanKeyspaces(cursor uint64, budget int, keep func(Keyspace, string) bool, dst []string) ([]string, uint64) {
	index, slot := int(cursor>>scanStoreShift), cursor&scanSlotMask
	for index < len(keyspaces) && keyspaces[index].Len() == 0 {
		index++
		slot = 0
	}
	if index >= len(keyspaces) {
		return dst, 0
	}
	ks := keyspaces[index]
	var next uint64
	dst, _, next = ks.Scan(slot, budget, func(key string) bool { return keep == nil || keep(ks, key) }, dst)
	if next != 0 {
		return dst, uint64(index)<<scanStoreShift | next
	}
	index++
	for index < len(keyspaces) && keyspaces[index].Len() == 0 {
		index++
	}
	if index >= len(keyspaces) {
		return dst, 0
	}
	return dst, uint64(index) << scanStoreShift
}

// KeyspaceWalk freezes slot high-water marks, not the key names or values.
// Mutations are permitted; rewrite's dirty reconciliation supplies the final
// state. Keys that survive the whole walk cannot move behind its cursor.
type KeyspaceWalk struct {
	ends    []uint64
	index   int
	cursor  uint64
	version uint64
}

func NewKeyspaceWalk() *KeyspaceWalk {
	w := &KeyspaceWalk{ends: make([]uint64, len(keyspaces)), version: keyspaceVersion}
	for i, ks := range keyspaces {
		w.ends[i] = ks.ScanEnd()
	}
	return w
}
func (w *KeyspaceWalk) Done() bool { return w.index >= len(w.ends) }
func (w *KeyspaceWalk) Next(budget int, dst []string) ([]string, int, error) {
	if w.version != keyspaceVersion {
		return dst, 0, fmt.Errorf("keyspace registry changed during traversal")
	}
	for !w.Done() && w.ends[w.index] == 0 {
		w.index++
	}
	if w.Done() {
		return dst, 0, nil
	}
	var examined int
	var next uint64
	dst, examined, next = keyspaces[w.index].ScanUntil(w.cursor, w.ends[w.index], budget, nil, dst)
	w.cursor = next
	if next == 0 {
		w.index++
		for !w.Done() && w.ends[w.index] == 0 {
			w.index++
		}
	}
	return dst, examined, nil
}
