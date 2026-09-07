package data_structure

import (
	"hash/maphash"
	"reflect"
)

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
	large      *hashIndex
}

func NewHash() *Hash {
	return &Hash{fields: make(map[string]string)}
}

// Set stores a field, reporting whether the field is new rather than whether
// anything changed. That is Redis's count for HSET: overwriting an existing
// field with a different value still answers 0.
func (h *Hash) Set(field, value string) bool {
	var old string
	var existed bool
	if h.large != nil {
		old, existed = h.large.set(field, value)
	} else {
		old, existed = h.fields[field]
		if !existed && len(h.fields) == hashLeafFields {
			h.large = &hashIndex{root: &hashNode{fields: h.fields, peak: len(h.fields)}, seed: maphash.MakeSeed(), count: len(h.fields)}
			h.fields = nil
			h.large.set(field, value)
		} else {
			h.fields[field] = value
		}
	}
	if existed {
		h.valueBytes -= uint64(len(old))
	} else {
		h.fieldBytes += uint64(len(field))
	}
	h.valueBytes += uint64(len(value))
	return !existed
}

func (h *Hash) Get(field string) (string, bool) {
	if h.large != nil {
		return h.large.get(field)
	}
	v, ok := h.fields[field]
	return v, ok
}

func (h *Hash) Exists(field string) bool {
	_, ok := h.Get(field)
	return ok
}

func (h *Hash) Del(fields ...string) int {
	removed := 0
	for _, f := range fields {
		var v string
		var ok bool
		if h.large == nil {
			v, ok = h.fields[f]
			if ok {
				delete(h.fields, f)
			}
		} else {
			v, ok = h.large.del(f)
			root := h.large.root
			if root == nil {
				h.fields, h.large = make(map[string]string), nil
			} else if root.fields != nil && root.next == nil {
				// Reuse the remaining small leaf without copying its fields.
				h.fields, h.large = root.fields, nil
			}
		}
		if !ok {
			continue
		}
		h.fieldBytes -= uint64(len(f))
		h.valueBytes -= uint64(len(v))
		removed++
	}
	return removed
}

func (h *Hash) Len() int {
	if h.large != nil {
		return h.large.count
	}
	return len(h.fields)
}

// HashCursor walks without first copying all field names. It belongs to the
// event-loop owner and must be discarded on any mutation of the hash. The
// rewrite owns at most one cursor; hashes retain no additional per-field index.
type HashCursor struct {
	iterator             reflect.MapIter
	valid                bool
	field, value         string
	fieldSlot, valueSlot reflect.Value
	pending              [65]*hashNode
	depth                int
}

func (h *Hash) Cursor() *HashCursor {
	c := &HashCursor{}
	c.fieldSlot = reflect.ValueOf(&c.field).Elem()
	c.valueSlot = reflect.ValueOf(&c.value).Elem()
	c.iterator.Reset(reflect.ValueOf(h.fields))
	if h.large != nil {
		c.pending[0], c.depth = h.large.root, 1
	}
	c.Advance()
	return c
}

func (c *HashCursor) Entry() (string, string, bool) {
	if !c.valid {
		return "", "", false
	}
	c.fieldSlot.SetIterKey(&c.iterator)
	c.valueSlot.SetIterValue(&c.iterator)
	return c.field, c.value, true
}

func (c *HashCursor) Advance() {
	c.valid = c.iterator.Next()
	if c.valid {
		return
	}
	for c.depth > 0 {
		c.depth--
		node := c.pending[c.depth]
		c.pending[c.depth] = nil
		if node.fields == nil {
			c.pending[c.depth], c.pending[c.depth+1] = node.left, node.right
			c.depth += 2
		} else {
			c.iterator.Reset(reflect.ValueOf(node.fields))
			if node.next != nil {
				c.pending[c.depth] = node.next
				c.depth++
			}
			c.valid = c.iterator.Next()
			if c.valid {
				return
			}
		}
	}
}

// Visit walks entries without allocating name/value arrays. The visitor may
// stop early; it must not mutate the hash during traversal.
func (h *Hash) Visit(yield func(field, value string) bool) {
	if h.large != nil {
		h.large.visit(yield)
		return
	}
	for field, value := range h.fields {
		if !yield(field, value) {
			return
		}
	}
}

// Fields, Values and Entries walk the map, so all three are in map order:
// arbitrary, and different every time. Redis says the same of HKEYS, HVALS and
// HGETALL, and the three are consistent with each other only within one call -
// which is why Entries exists rather than callers pairing Fields with Values.
func (h *Hash) Fields() []string {
	out := make([]string, 0, h.Len())
	h.Visit(func(f, _ string) bool { out = append(out, f); return true })
	return out
}

func (h *Hash) Values() []string {
	out := make([]string, 0, h.Len())
	h.Visit(func(_, v string) bool { out = append(out, v); return true })
	return out
}

// Entries returns fields and values in one pass, positionally matched.
func (h *Hash) Entries() ([]string, []string) {
	fields := make([]string, 0, h.Len())
	values := make([]string, 0, h.Len())
	h.Visit(func(f, v string) bool {
		fields = append(fields, f)
		values = append(values, v)
		return true
	})
	return fields, values
}

// MemUsage estimates the bytes held, in O(1).
//
// The per-field overhead is measured rather than derived, the same way the
// set's was - see TestHashMemUsageTracksRealHeap, which fails if this stops
// describing the map underneath.
func (h *Hash) MemUsage() uint64 {
	return hashBaseBytes + uint64(h.Len())*hashFieldOverhead +
		h.fieldBytes + h.valueBytes
}
