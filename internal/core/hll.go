package core

import "github.com/brandopakel/keel/internal/data_structure"

// Typed operations: HyperLogLog (phase 4 of docs/embedding-plan.md,
// converted as docs/phase4-family-recipe.md converts a family).
//
// Each operation runs in a command's scope, after the type check of the keys
// it names, and stages the record of every write it makes, so the
// HyperLogLog commands are never logged as they arrived (see strings.go).
// The records are the bytes the as-arrived log held: PFADD and PFMERGE
// record themselves, their names and keys and elements as given.

// opPFAdd adds items to the HyperLogLog key holds, creating it, and reports
// whether that changed it: PFADD. Creating the key is itself a change, so
// PFADD with no items reports true the first time.
//
// It is recorded as it was asked for when it changed the sketch, and not
// otherwise, as Redis propagates it (hyperloglog.c, pfaddCommand:
// server.dirty only when a register was updated or the key created).
func (e *Engine) opPFAdd(key string, items []string) bool {
	hll, exist := e.hllStore.Get(key)
	changed := !exist
	if !exist {
		hll = data_structure.CreateHLL()
		e.hllStore.Put(key, hll)
	}
	for _, item := range items {
		if hll.Add(item) {
			changed = true
		}
	}
	e.hllStore.Resize(key)
	if changed {
		e.aofRecordKeyArgs("PFADD", key, items)
	}
	return changed
}

// opPFCount is the estimated cardinality of the HyperLogLog key holds, or of
// the union of those keys hold when there are several, zero for a key that
// holds none: PFCOUNT. A union is computed into a temporary sketch rather
// than by merging into the first key, because PFCOUNT is a read: the stored
// sketches come out unchanged.
func (e *Engine) opPFCount(keys []string) int64 {
	if len(keys) == 1 {
		if hll, exist := e.hllStore.Get(keys[0]); exist {
			return int64(hll.Count())
		}
		return 0
	}
	union := data_structure.CreateHLL()
	for _, key := range keys {
		if hll, exist := e.hllStore.Get(key); exist {
			union.Merge(hll)
		}
	}
	return int64(union.Count())
}

// opPFMerge stores at dest the union of the HyperLogLogs dest and sources
// hold: PFMERGE. The destination takes part in its own merge, so PFMERGE
// accumulates rather than replacing what is already there. It is recorded as
// it was asked for.
func (e *Engine) opPFMerge(dest string, sources []string) {
	merged := data_structure.CreateHLL()
	if existing, exist := e.hllStore.Get(dest); exist {
		merged.Merge(existing)
	}
	for _, key := range sources {
		if hll, exist := e.hllStore.Get(key); exist {
			merged.Merge(hll)
		}
	}
	e.hllStore.Put(dest, merged)
	e.aofRecordKeyArgs("PFMERGE", dest, sources)
}
