package data_structure

// Rebuild only the hash-to-slot lookup table. Stable slots, collision lists and
// active traversal cursors keep their identity throughout compaction.
type lookupCompaction struct {
	next        map[uint64]uint64
	cursor, end uint64
}

func (m *keyMap[V]) compact(budget int) int {
	if budget <= 0 || m.count == 0 {
		return 0
	}
	if m.compaction != nil && len(m.lookup) > m.lookupPeak/2 {
		m.compaction = nil
		return 0
	}
	if m.compaction == nil {
		if m.lookupPeak < 1024 || len(m.lookup) > m.lookupPeak/4 {
			return 0
		}
		m.compaction = &lookupCompaction{next: make(map[uint64]uint64), end: m.end}
	}
	task := m.compaction
	_, work, next := m.scanUntil(task.cursor, task.end, budget, func(key string) bool {
		_, hash, ok := m.position(key)
		if ok {
			task.next[hash] = m.lookup[hash]
		}
		return false
	}, nil)
	task.cursor = next
	if next == 0 {
		m.lookup = task.next
		m.lookupPeak = len(m.lookup)
		m.compaction = nil
	}
	return work
}

func (d *Dict) CompactLookup(budget int) int     { return d.dictStore.compact(budget) }
func (k *Keyed[T]) CompactLookup(budget int) int { return k.items.compact(budget) }
