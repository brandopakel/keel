package data_structure

// expiryCompaction builds a smaller TTL table across maintenance turns. The
// original remains authoritative until the bounded traversal completes. Every
// intervening TTL update/removal is mirrored, including keys inserted into
// already visited slots or beyond the captured traversal end.
type expiryCompaction struct {
	next        map[string]uint64
	cursor, end uint64
}

func compactExpiry(source *map[string]uint64, peak *int, job **expiryCompaction, space Keyspace, budget int) int {
	if budget <= 0 || len(*source) == 0 {
		return 0
	}
	if *job != nil && len(*source) > (*peak)/2 {
		*job = nil
		return 0
	}
	if *job == nil {
		if *peak < 1024 || len(*source) > (*peak)/4 {
			return 0
		}
		// No dataset-sized allocation hint: the replacement grows with each step.
		*job = &expiryCompaction{next: make(map[string]uint64), end: space.ScanEnd()}
	}
	task := *job
	_, work, next := space.ScanUntil(task.cursor, task.end, budget, func(key string) bool {
		if at, ok := (*source)[key]; ok {
			task.next[key] = at
		}
		return false // never materialize a key-name batch
	}, nil)
	task.cursor = next
	if next == 0 {
		*source = task.next
		*peak = len(task.next)
		*job = nil
	}
	return work
}

func (d *Dict) CompactExpiry(budget int) int {
	return compactExpiry(&d.expiredDictStore, &d.expiryPeak, &d.expiryCompaction, d, budget)
}
func (k *Keyed[T]) CompactExpiry(budget int) int {
	return compactExpiry(&k.expiries, &k.expiryPeak, &k.expiryCompaction, k, budget)
}
