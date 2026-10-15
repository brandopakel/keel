package core

import (
	"strconv"

	"github.com/brandopakel/keel/internal/data_structure"
)

// Typed operations: Count-Min sketches (phase 4 of docs/embedding-plan.md,
// converted as docs/phase4-family-recipe.md converts a family), with the
// names and replies of RedisBloom's CMS.* family.
//
// Each operation runs after the type check of the key it names and stages
// the record of every write it makes. A sketch's record is the command as
// RedisBloom replicates it, verbatim (RedisModule_ReplicateVerbatim): its
// name, its key, and its arguments as the client wrote them, which an
// operation is handed as text, since a number has more than one spelling and
// the log holds the client's. A Go caller has no text, and its record is
// written from the values.

// formatUint32s is values in decimal, the text a Go caller's record holds.
func formatUint32s(values ...uint32) []string {
	text := make([]string, len(values))
	for i, v := range values {
		text[i] = strconv.FormatUint(uint64(v), 10)
	}
	return text
}

// formatRates is rates as a record holds them for a Go caller.
func formatRates(rates ...float64) []string {
	text := make([]string, len(rates))
	for i, r := range rates {
		text[i] = strconv.FormatFloat(r, 'g', -1, 64)
	}
	return text
}

func (e *Engine) cmsFor(key string) (*data_structure.CMS, bool) {
	return e.cmsStore.Get(key)
}

// cmsCreate stores a fresh sketch at key, refusing dimensions that are zero or
// that would not fit, and a key already taken. The dimensions arrive from a
// client and multiply into an allocation, so their product is checked against
// what the server will allocate before anything is made.
func (e *Engine) cmsCreate(key string, width, depth uint32) error {
	if width == 0 {
		return errCMSWidth
	}
	if depth == 0 {
		return errCMSDepth
	}
	// The cell count is checked before the byte count: two 32-bit dimensions
	// multiply to a number of cells that fits in 64 bits, but four bytes each
	// can wrap, and a wrapped size would look small.
	if uint64(width)*uint64(depth) > maxStructureBytes/4 {
		return errTooLargeForOneKey
	}
	if err := e.affordable(data_structure.CMSMemUsageFor(width, depth)); err != nil {
		return err
	}
	if e.cmsStore.Exists(key) {
		return errCMSExists
	}
	e.cmsStore.Put(key, data_structure.CreateCMS(width, depth))
	return nil
}

// opCMSInitByDim creates a sketch of width by depth counters at key:
// CMS.INITBYDIM. text is the width and depth as a client wrote them.
func (e *Engine) opCMSInitByDim(key string, width, depth uint32, text []string) error {
	if err := e.cmsCreate(key, width, depth); err != nil {
		return err
	}
	if text == nil {
		text = formatUint32s(width, depth)
	}
	e.aofRecordKeyArgs("CMS.INITBYDIM", key, text)
	return nil
}

// opCMSInitByProb creates a sketch at key sized so an estimate is within
// errRate of the total count with probability 1 - prob: CMS.INITBYPROB. text
// is the two rates as a client wrote them.
func (e *Engine) opCMSInitByProb(key string, errRate, prob float64, text []string) error {
	width, depth := data_structure.CalcCMSDim(errRate, prob)
	if err := e.cmsCreate(key, width, depth); err != nil {
		return err
	}
	if text == nil {
		text = formatRates(errRate, prob)
	}
	e.aofRecordKeyArgs("CMS.INITBYPROB", key, text)
	return nil
}

// opCMSIncrBy adds by[i] to items[i]'s counters in the sketch key holds and
// hands each the estimate after it, or saturated for a counter that cannot
// take the increment: CMS.INCRBY. A key that holds no sketch is
// errCMSMissing. text is the items and increments as a client wrote them,
// item, increment, item, increment.
func (e *Engine) opCMSIncrBy(key string, items []string, by []uint32, text []string, each func(estimate uint32, saturated bool)) error {
	cms, ok := e.cmsFor(key)
	if !ok {
		return errCMSMissing
	}
	for i, item := range items {
		each(cms.IncrBy(item, by[i]))
	}
	e.cmsStore.Modified(key)
	if text == nil {
		text = make([]string, 0, 2*len(items))
		for i, item := range items {
			text = append(text, item, strconv.FormatUint(uint64(by[i]), 10))
		}
	}
	e.aofRecordKeyArgs("CMS.INCRBY", key, text)
	return nil
}

// opCMSQuery hands each of items' estimates in the sketch key holds, in
// order: CMS.QUERY. A key that holds no sketch is errCMSMissing.
func (e *Engine) opCMSQuery(key string, items []string, each func(count uint32)) error {
	cms, ok := e.cmsFor(key)
	if !ok {
		return errCMSMissing
	}
	for _, item := range items {
		each(cms.Count(item))
	}
	return nil
}
