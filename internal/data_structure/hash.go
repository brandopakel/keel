package data_structure

// Hash is a field-value map under one key, the type behind HSET and HGET.
//
// A plain Go map, like the set. The interesting part is not the structure but
// the accounting: field and value lengths are maintained as they change so
// MemUsage stays O(1). Summing on demand would make the memory-budget check
// proportional to the size of the hash, and that check runs on every write.
type Hash struct {
	fields map[string]string
	// fieldBytes and valueBytes are the totals of the names and the values
	// held. Kept apart only because it makes the arithmetic on overwrite
	// legible: a Set replaces a value's bytes and leaves the field's alone.
	fieldBytes uint64
	valueBytes uint64
}

func NewHash() *Hash {
	return &Hash{fields: make(map[string]string)}
}

// Set stores a field, reporting whether the field is new rather than whether
// anything changed. That is Redis's count for HSET: overwriting an existing
// field with a different value still answers 0.
func (h *Hash) Set(field, value string) bool {
	old, existed := h.fields[field]
	if existed {
		h.valueBytes -= uint64(len(old))
	} else {
		h.fieldBytes += uint64(len(field))
	}
	h.fields[field] = value
	h.valueBytes += uint64(len(value))
	return !existed
}

func (h *Hash) Get(field string) (string, bool) {
	v, ok := h.fields[field]
	return v, ok
}

func (h *Hash) Exists(field string) bool {
	_, ok := h.fields[field]
	return ok
}

func (h *Hash) Del(fields ...string) int {
	removed := 0
	for _, f := range fields {
		v, ok := h.fields[f]
		if !ok {
			continue
		}
		delete(h.fields, f)
		h.fieldBytes -= uint64(len(f))
		h.valueBytes -= uint64(len(v))
		removed++
	}
	return removed
}

func (h *Hash) Len() int { return len(h.fields) }

// Fields, Values and Entries walk the map, so all three are in map order:
// arbitrary, and different every time. Redis says the same of HKEYS, HVALS and
// HGETALL, and the three are consistent with each other only within one call -
// which is why Entries exists rather than callers pairing Fields with Values.
func (h *Hash) Fields() []string {
	out := make([]string, 0, len(h.fields))
	for f := range h.fields {
		out = append(out, f)
	}
	return out
}

func (h *Hash) Values() []string {
	out := make([]string, 0, len(h.fields))
	for _, v := range h.fields {
		out = append(out, v)
	}
	return out
}

// Entries returns fields and values in one pass, positionally matched.
func (h *Hash) Entries() ([]string, []string) {
	fields := make([]string, 0, len(h.fields))
	values := make([]string, 0, len(h.fields))
	for f, v := range h.fields {
		fields = append(fields, f)
		values = append(values, v)
	}
	return fields, values
}

// MemUsage estimates the bytes held, in O(1).
//
// The per-field overhead is measured rather than derived, the same way the
// set's was - see TestHashMemUsageTracksRealHeap, which fails if this stops
// describing the map underneath.
func (h *Hash) MemUsage() uint64 {
	return hashBaseBytes + uint64(len(h.fields))*hashFieldOverhead +
		h.fieldBytes + h.valueBytes
}
