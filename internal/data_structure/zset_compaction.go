package data_structure

// The existing skip list provides a live traversal independent of sparsely
// occupied Go map buckets. The old dictionary stays authoritative throughout.
// Mutation hooks mirror new scores and removals, and move the cursor before its
// node is removed or rescored. No worker reads mutable collection state.
type zsetIndexState struct {
	peak, initialLen, visited int
	next                      map[string]float64
	cursor                    *skiplistNode
}

func (zs *ZSet) indexWritten(member string, score float64) {
	c := zs.indexState
	if c == nil {
		if len(zs.dict) < 1024 {
			return
		}
		c = &zsetIndexState{}
		zs.indexState = c
	}
	c.peak = max(c.peak, len(zs.dict))
	if c.next != nil {
		if len(zs.dict) > 2*c.initialLen {
			c.abandon()
		} else {
			c.next[member] = score
		}
	}
}

func (zs *ZSet) advanceIndexCursor(member string) {
	if c := zs.indexState; c != nil && c.cursor != nil && c.cursor.ele == member {
		// Even an in-place score update may skip this node: indexWritten mirrors
		// its new score. Keeping a detached node would retain stale links.
		c.cursor = c.cursor.next()
	}
}

func (zs *ZSet) indexRemoved(member string) {
	c := zs.indexState
	if c == nil {
		return
	}
	n := len(zs.dict)
	if n == 0 {
		zs.dict = make(map[string]float64)
		zs.indexState = nil
		return
	}
	if c.next != nil {
		delete(c.next, member)
	}
	if (c.next == nil && n <= c.peak/4) || (c.next != nil && n < c.initialLen/4) {
		// Do not preallocate for the entire population on the command path.
		// The replacement grows only as bounded maintenance copies entries.
		c.next = make(map[string]float64)
		c.cursor = zs.sl.first()
		c.initialLen, c.visited = n, 0
	}
}

func (c *zsetIndexState) abandon() {
	c.next, c.cursor = nil, nil
	c.initialLen, c.visited = 0, 0
}

// CompactIndex charges one unit per live skip-list node copied, capped by the
// shared maintenance budget. Changes can insert nodes ahead of the cursor, so
// abort after twice the initial population rather than chase indefinite churn.
// Only reaching the end permits publication. A later removal may retry an
// abandoned rebuild; quiescent sparse collections complete in bounded slices.
func (zs *ZSet) CompactIndex(budget int) int {
	c := zs.indexState
	if c == nil || c.next == nil || budget <= 0 {
		return 0
	}
	work := 0
	for c.cursor != nil && work < min(budget, ScanMaxWork) {
		node := c.cursor
		c.next[node.ele] = node.score
		c.cursor = node.next()
		c.visited++
		work++
		if c.cursor != nil && c.visited >= 2*c.initialLen {
			c.abandon()
			return work
		}
	}
	if c.cursor == nil {
		zs.dict = c.next
		c.peak = len(zs.dict)
		c.abandon()
		if c.peak < 1024 {
			zs.indexState = nil
		}
	}
	return work
}
