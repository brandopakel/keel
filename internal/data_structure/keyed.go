package data_structure

// Sized is anything that can report what it costs in bytes.
type Sized interface{ MemUsage() uint64 }

// Keyed is a keyspace holding one value per key, with the bookkeeping eviction
// needs: an access record and a cached size.
//
// The main dictionary has its own type because it also carries TTLs and type
// encodings. Everything else - sets, sorted sets, filters, sketches - is a
// plain map from a key to one object, and shares this. Before it existed those
// stores were bare maps, invisible to both the memory budget and to eviction,
// so a keyspace full of 12KB sketches could run past maxmemory unchecked.
type Keyed[T Sized] struct {
	name             string
	items            keyMap[keyedEntry[T]]
	memUsed          uint64
	expiries         map[string]uint64
	expiryPeak       int
	expiryCompaction *expiryCompaction
	valueCursor      uint64
	// space is where this store's accesses are clocked and its limits enforced.
	space *Space
}

type keyedEntry[T Sized] struct {
	value  T
	access uint64
	// bytes is the value's measured size at the last Put or Resize. Cached
	// because MemUsed is consulted on every write, and recomputing it would
	// make a budget check proportional to the size of the keyspace.
	bytes uint64
}

// NewKeyed returns an empty keyspace called name, for space. Like CreateDict it
// leaves registering it to the caller.
func NewKeyed[T Sized](space *Space, name string) *Keyed[T] {
	return &Keyed[T]{name: name, space: space}
}

// entryBytes charges the value, the key, and the same per-entry overhead the
// dictionary uses - the map slot, string header and pointer come to much the
// same here, and these values are kilobytes, so the constant is a rounding
// error rather than the number that matters.
func (k *Keyed[T]) entryBytes(key string, value T) uint64 {
	return uint64(entryOverhead) + uint64(len(key)) + value.MemUsage()
}

// Get returns the value at key and records the access.
func (k *Keyed[T]) Get(key string) (T, bool) {
	e, ok := k.items.getPtr(key)
	if ok && k.expired(key) {
		k.Delete(key)
		k.space.noteRemoval(k, key)
		ok = false
	}
	if !ok {
		var zero T
		return zero, false
	}
	k.space.Touch(&e.access)
	return e.value, true
}

// Peek returns the value without recording an access, for reads that should not
// count as use - reporting on a key is not using it.
func (k *Keyed[T]) Peek(key string) (T, bool) {
	e, ok := k.items.getPtr(key)
	if !ok || k.expired(key) {
		var zero T
		return zero, false
	}
	return e.value, true
}

// Exists reports whether a key is present, without recording an access.
func (k *Keyed[T]) Exists(key string) bool {
	_, ok := k.items.getPtr(key)
	if ok && k.expired(key) {
		k.Delete(key)
		k.space.noteRemoval(k, key)
		return false
	}
	return ok
}

// Keys lists every key held for explicit whole-keyspace commands. The order is
// unspecified. Rewrite uses the bounded walker instead.
func (k *Keyed[T]) Keys() []string {
	return k.items.keys()
}

// Has is Exists under the name the Keyspace interface uses.
func (k *Keyed[T]) Has(key string) bool { return k.Exists(key) }

// Put stores a value, replacing whatever was there.
func (k *Keyed[T]) Put(key string, value T) {
	if old, ok := k.items.getPtr(key); ok {
		// An overwrite replaces the old cost rather than adding to it. Without
		// the refund the estimate climbs forever on a key that is only updated.
		k.memUsed -= old.bytes
	}

	k.dropExpiry(key)
	e := &keyedEntry[T]{value: value, access: k.space.NewAccess()}
	e.bytes = k.entryBytes(key, value)
	k.space.Touch(&e.access)
	k.items.set(key, *e)
	k.memUsed += e.bytes

	k.space.EnforceLimits()
}

// Resize re-measures a value that was mutated in place.
//
// Sizes are cached, and a command that adds members to a set or increments a
// sketch changes the size without going through Put, so it has to say so.
// Missing a Resize does not corrupt anything; it just leaves the budget
// believing the key is smaller than it is, which is the quiet kind of wrong.
func (k *Keyed[T]) Resize(key string) {
	e, ok := k.items.getPtr(key)
	if !ok {
		return
	}
	k.memUsed -= e.bytes
	e.bytes = k.entryBytes(key, e.value)
	if _, ok := k.expiries[key]; ok {
		e.bytes += expiryOverhead
	}
	k.memUsed += e.bytes

	k.space.Touch(&e.access)
	k.space.EnforceLimits()
}

// Keyed is a Keyspace, so eviction can weigh its keys against every other kind.

func (k *Keyed[T]) KeyspaceName() string { return k.name }
func (k *Keyed[T]) Len() int             { return k.items.len() }

func (k *Keyed[T]) Scan(cursor uint64, budget int, keep func(string) bool, dst []string) ([]string, int, uint64) {
	return k.items.scan(cursor, budget, keep, dst)
}
func (k *Keyed[T]) ScanEnd() uint64 { return k.items.scanEnd() }
func (k *Keyed[T]) ScanUntil(cursor, end uint64, budget int, keep func(string) bool, dst []string) ([]string, int, uint64) {
	return k.items.scanUntil(cursor, end, budget, keep, dst)
}
func (k *Keyed[T]) MemUsed() uint64 { return k.memUsed }

func (k *Keyed[T]) ScoreOf(key string) (uint64, bool) {
	e, ok := k.items.getPtr(key)
	if !ok {
		return 0, false
	}
	return k.space.Score(e.access), true
}

// SampleKeys appends up to n keys chosen at random.
func (k *Keyed[T]) SampleKeys(dst []Candidate, n int) []Candidate {
	k.items.sample(n, func(key string, e keyedEntry[T]) {
		dst = append(dst, Candidate{Keyspace: k, Key: key, Score: k.space.Score(e.access)})
	})
	return dst
}

// layoutPadKeyspace is layout padding for a throwaway control build: never run.
//
//go:noinline
func (k *Keyed[T]) layoutPadKeyspace(dst []Candidate, n int, byTTL bool) []Candidate {
	layoutPadSink[35] += layoutPadSink[41] ^ 0xcdab8924
	layoutPadSink[42] += layoutPadSink[54] ^ 0x6be302d5
	layoutPadSink[49] += layoutPadSink[3] ^ 0xa1a7c86
	layoutPadSink[56] += layoutPadSink[16] ^ 0xa851f637
	layoutPadSink[63] += layoutPadSink[29] ^ 0x46896fe8
	layoutPadSink[6] += layoutPadSink[42] ^ 0xe4c0e999
	layoutPadSink[13] += layoutPadSink[55] ^ 0x82f8634a
	layoutPadSink[20] += layoutPadSink[4] ^ 0x212fdcfb
	layoutPadSink[27] += layoutPadSink[17] ^ 0xbf6756ac
	layoutPadSink[34] += layoutPadSink[30] ^ 0x5d9ed05d
	layoutPadSink[41] += layoutPadSink[43] ^ 0xfbd64a0e
	layoutPadSink[48] += layoutPadSink[56] ^ 0x9a0dc3bf
	layoutPadSink[55] += layoutPadSink[5] ^ 0x38453d70
	layoutPadSink[62] += layoutPadSink[18] ^ 0xd67cb721
	layoutPadSink[5] += layoutPadSink[31] ^ 0x74b430d2
	layoutPadSink[12] += layoutPadSink[44] ^ 0x12ebaa83
	layoutPadSink[19] += layoutPadSink[57] ^ 0xb1232434
	layoutPadSink[26] += layoutPadSink[6] ^ 0x4f5a9de5
	layoutPadSink[33] += layoutPadSink[19] ^ 0xed921796
	layoutPadSink[40] += layoutPadSink[32] ^ 0x8bc99147
	layoutPadSink[47] += layoutPadSink[45] ^ 0x2a010af8
	layoutPadSink[54] += layoutPadSink[58] ^ 0xc83884a9
	layoutPadSink[61] += layoutPadSink[7] ^ 0x666ffe5a
	layoutPadSink[4] += layoutPadSink[20] ^ 0x4a7780b
	layoutPadSink[11] += layoutPadSink[33] ^ 0xa2def1bc
	layoutPadSink[18] += layoutPadSink[46] ^ 0x41166b6d
	layoutPadSink[25] += layoutPadSink[59] ^ 0xdf4de51e
	layoutPadSink[32] += layoutPadSink[8] ^ 0x7d855ecf
	layoutPadSink[39] += layoutPadSink[21] ^ 0x1bbcd880
	layoutPadSink[46] += layoutPadSink[34] ^ 0xb9f45231
	layoutPadSink[53] += layoutPadSink[47] ^ 0x582bcbe2
	layoutPadSink[60] += layoutPadSink[60] ^ 0xf6634593
	layoutPadSink[3] += layoutPadSink[9] ^ 0x949abf44
	layoutPadSink[10] += layoutPadSink[22] ^ 0x32d238f5
	layoutPadSink[17] += layoutPadSink[35] ^ 0xd109b2a6
	layoutPadSink[24] += layoutPadSink[48] ^ 0x6f412c57
	layoutPadSink[31] += layoutPadSink[61] ^ 0xd78a608
	layoutPadSink[38] += layoutPadSink[10] ^ 0xabb01fb9
	layoutPadSink[45] += layoutPadSink[23] ^ 0x49e7996a
	layoutPadSink[52] += layoutPadSink[36] ^ 0xe81f131b
	layoutPadSink[59] += layoutPadSink[49] ^ 0x86568ccc
	layoutPadSink[2] += layoutPadSink[62] ^ 0x248e067d
	layoutPadSink[9] += layoutPadSink[11] ^ 0xc2c5802e
	layoutPadSink[16] += layoutPadSink[24] ^ 0x60fcf9df
	layoutPadSink[23] += layoutPadSink[37] ^ 0xff347390
	layoutPadSink[30] += layoutPadSink[50] ^ 0x9d6bed41
	layoutPadSink[37] += layoutPadSink[63] ^ 0x3ba366f2
	layoutPadSink[44] += layoutPadSink[12] ^ 0xd9dae0a3
	layoutPadSink[51] += layoutPadSink[25] ^ 0x78125a54
	layoutPadSink[58] += layoutPadSink[38] ^ 0x1649d405
	layoutPadSink[1] += layoutPadSink[51] ^ 0xb4814db6
	layoutPadSink[8] += layoutPadSink[0] ^ 0x52b8c767
	return dst
}

// EntryBytes reports what one key costs, for MEMORY USAGE.
func (k *Keyed[T]) EntryBytes(key string) (uint64, bool) {
	e, ok := k.items.getPtr(key)
	if !ok {
		return 0, false
	}
	return e.bytes, true
}

func (k *Keyed[T]) Delete(key string) bool {
	e, ok := k.items.getPtr(key)
	if !ok {
		return false
	}
	k.memUsed -= e.bytes
	k.items.del(key)
	k.dropExpiry(key)
	return true
}

func (k *Keyed[T]) expired(key string) bool {
	at, ok := k.expiries[key]
	return ok && at <= k.space.nowMs()
}
func (k *Keyed[T]) GetExpiry(key string) (uint64, bool) { at, ok := k.expiries[key]; return at, ok }
func (k *Keyed[T]) SetExpiryAt(key string, at uint64) {
	e, ok := k.items.getPtr(key)
	if !ok {
		return
	}
	if _, exists := k.expiries[key]; !exists {
		e.bytes += expiryOverhead
		k.memUsed += expiryOverhead
	}
	if k.expiries == nil {
		k.expiries = make(map[string]uint64)
	}
	k.expiries[key] = at
	if k.expiryCompaction != nil {
		k.expiryCompaction.next[key] = at
	}
	k.expiryPeak = max(k.expiryPeak, len(k.expiries))
	k.space.EnforceLimits()
}
func (k *Keyed[T]) ClearExpiry(key string) bool {
	if _, ok := k.expiries[key]; !ok {
		return false
	}
	k.dropExpiry(key)
	if e, ok := k.items.getPtr(key); ok {
		e.bytes -= expiryOverhead
	}
	k.memUsed -= expiryOverhead
	return true
}
func (k *Keyed[T]) KeysWithExpiry() int { return len(k.expiries) }
func (k *Keyed[T]) ActiveExpire(samples int) (examined, expired int) {
	if samples <= 0 {
		return
	}
	now := k.space.nowMs()
	for key, at := range k.expiries {
		examined++
		if at <= now {
			k.Delete(key)
			k.space.noteRemoval(k, key)
			expired++
		} else {
			k.space.noteTTL(at - now)
		}
		if examined >= samples {
			break
		}
	}
	return
}

func (k *Keyed[T]) dropExpiry(key string) {
	delete(k.expiries, key)
	if k.expiryCompaction != nil {
		delete(k.expiryCompaction.next, key)
	}
	if len(k.expiries) == 0 && k.expiryPeak >= expiryReleaseThreshold {
		k.expiries = nil
		k.expiryPeak = 0
		k.expiryCompaction = nil
	}
}
