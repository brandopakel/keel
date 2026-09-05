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
	name     string
	items    map[string]*keyedEntry[T]
	memUsed  uint64
	expiries map[string]uint64
}

type keyedEntry[T Sized] struct {
	value  T
	access uint64
	// bytes is the value's measured size at the last Put or Resize. Cached
	// because MemUsed is consulted on every write, and recomputing it would
	// make a budget check proportional to the size of the keyspace.
	bytes uint64
}

func NewKeyed[T Sized](name string) *Keyed[T] {
	return &Keyed[T]{name: name, items: make(map[string]*keyedEntry[T]), expiries: make(map[string]uint64)}
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
	e, ok := k.items[key]
	if ok && k.expired(key) {
		k.Delete(key)
		noteRemoval(k, key)
		ok = false
	}
	if !ok {
		var zero T
		return zero, false
	}
	Touch(&e.access)
	return e.value, true
}

// Peek returns the value without recording an access, for reads that should not
// count as use - reporting on a key is not using it.
func (k *Keyed[T]) Peek(key string) (T, bool) {
	e, ok := k.items[key]
	if !ok || k.expired(key) {
		var zero T
		return zero, false
	}
	return e.value, true
}

// Exists reports whether a key is present, without recording an access.
func (k *Keyed[T]) Exists(key string) bool {
	_, ok := k.items[key]
	if ok && k.expired(key) {
		k.Delete(key)
		noteRemoval(k, key)
		return false
	}
	return ok
}

// Keys lists every key held, for a rewrite that has to walk the whole store.
// The order is a map's, which is to say arbitrary and different every time -
// which is fine, because a log is replayed as a whole and the keys in it do not
// interact.
func (k *Keyed[T]) Keys() []string {
	keys := make([]string, 0, len(k.items))
	for key := range k.items {
		keys = append(keys, key)
	}
	return keys
}

// Has is Exists under the name the Keyspace interface uses.
func (k *Keyed[T]) Has(key string) bool { return k.Exists(key) }

// Put stores a value, replacing whatever was there.
func (k *Keyed[T]) Put(key string, value T) {
	if old, ok := k.items[key]; ok {
		// An overwrite replaces the old cost rather than adding to it. Without
		// the refund the estimate climbs forever on a key that is only updated.
		k.memUsed -= old.bytes
	}

	delete(k.expiries, key)
	e := &keyedEntry[T]{value: value, access: NewAccess()}
	e.bytes = k.entryBytes(key, value)
	k.items[key] = e
	k.memUsed += e.bytes

	Touch(&e.access)
	EnforceLimits()
}

// Resize re-measures a value that was mutated in place.
//
// Sizes are cached, and a command that adds members to a set or increments a
// sketch changes the size without going through Put, so it has to say so.
// Missing a Resize does not corrupt anything; it just leaves the budget
// believing the key is smaller than it is, which is the quiet kind of wrong.
func (k *Keyed[T]) Resize(key string) {
	e, ok := k.items[key]
	if !ok {
		return
	}
	k.memUsed -= e.bytes
	e.bytes = k.entryBytes(key, e.value)
	if _, ok := k.expiries[key]; ok {
		e.bytes += expiryOverhead
	}
	k.memUsed += e.bytes

	Touch(&e.access)
	EnforceLimits()
}

// Keyed is a Keyspace, so eviction can weigh its keys against every other kind.

func (k *Keyed[T]) KeyspaceName() string { return k.name }
func (k *Keyed[T]) Len() int             { return len(k.items) }
func (k *Keyed[T]) MemUsed() uint64      { return k.memUsed }

func (k *Keyed[T]) ScoreOf(key string) (uint64, bool) {
	e, ok := k.items[key]
	if !ok {
		return 0, false
	}
	return Score(e.access), true
}

// SampleKeys appends up to n keys chosen at random.
func (k *Keyed[T]) SampleKeys(dst []Candidate, n int) []Candidate {
	taken := 0
	for key, e := range k.items {
		dst = append(dst, Candidate{Space: k, Key: key, Score: Score(e.access)})
		if taken++; taken >= n {
			break
		}
	}
	return dst
}

// EntryBytes reports what one key costs, for MEMORY USAGE.
func (k *Keyed[T]) EntryBytes(key string) (uint64, bool) {
	e, ok := k.items[key]
	if !ok {
		return 0, false
	}
	return e.bytes, true
}

func (k *Keyed[T]) Delete(key string) bool {
	e, ok := k.items[key]
	if !ok {
		return false
	}
	k.memUsed -= e.bytes
	delete(k.items, key)
	delete(k.expiries, key)
	return true
}

func (k *Keyed[T]) expired(key string) bool             { at, ok := k.expiries[key]; return ok && at <= nowMs() }
func (k *Keyed[T]) GetExpiry(key string) (uint64, bool) { at, ok := k.expiries[key]; return at, ok }
func (k *Keyed[T]) SetExpiryAt(key string, at uint64) {
	e, ok := k.items[key]
	if !ok {
		return
	}
	if _, exists := k.expiries[key]; !exists {
		e.bytes += expiryOverhead
		k.memUsed += expiryOverhead
	}
	k.expiries[key] = at
	EnforceLimits()
}
func (k *Keyed[T]) ClearExpiry(key string) bool {
	if _, ok := k.expiries[key]; !ok {
		return false
	}
	delete(k.expiries, key)
	k.items[key].bytes -= expiryOverhead
	k.memUsed -= expiryOverhead
	return true
}
func (k *Keyed[T]) KeysWithExpiry() int { return len(k.expiries) }
func (k *Keyed[T]) ActiveExpire(samples int) (examined, expired int) {
	if samples <= 0 {
		return
	}
	now := nowMs()
	for key, at := range k.expiries {
		examined++
		if at <= now {
			k.Delete(key)
			noteRemoval(k, key)
			expired++
		}
		if examined >= samples {
			break
		}
	}
	return
}
