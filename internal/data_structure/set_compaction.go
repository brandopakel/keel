package data_structure

// Rebuild the membership map from the set's dense order slice. Mutations mirror
// moved, shuffled, inserted and deleted positions while the old map remains
// authoritative. No scan over sparsely occupied Go map buckets is needed.
type setIndexCompaction struct {
	next               map[string]int
	cursor, initialLen int
}

func (s *Set) setIndex(member string, position int) {
	s.index[member] = position
	if s.compaction != nil {
		s.compaction.next[member] = position
	}
}

// CompactIndex charges one unit per dense member position visited. It changes
// neither logical membership nor member ordering and needs no persistence record.
func (s *Set) CompactIndex(budget int) int {
	c := s.compaction
	if c == nil || budget <= 0 {
		return 0
	}
	budget = min(budget, ScanMaxWork)
	work := 0
	for c.cursor < min(c.initialLen, len(s.order)) && work < budget {
		c.next[s.order[c.cursor]] = c.cursor
		c.cursor++
		work++
	}
	if c.cursor >= min(c.initialLen, len(s.order)) {
		s.index = c.next
		s.compaction = nil
	}
	return work
}

// CompactValues advances collection-internal maintenance through stable key
// slots. Each empty page/key examined and each member copied shares one budget.
// A large active value can consume the rest of a turn; later turns resume at
// the next key so other collections continue making progress.
func (k *Keyed[T]) CompactValues(budget int) int {
	budget = min(max(budget, 0), ScanMaxWork)
	work := 0
	var names [1]string
	for work < budget {
		keys, used, next := k.items.scan(k.valueCursor, 1, nil, names[:0])
		k.valueCursor = next
		work += used
		if len(keys) > 0 {
			if entry, ok := k.items.getPtr(keys[0]); ok {
				if value, ok := any(entry.value).(interface{ CompactIndex(int) int }); ok {
					work += value.CompactIndex(budget - work)
				}
			}
		}
		if next == 0 {
			break
		}
	}
	return work
}
