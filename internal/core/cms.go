package core

import (
	"math"
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

// cmsCreate stores a fresh sketch of width by depth counters at key, as
// RedisBloom's CMSketch_Create does once it has read them: a key that holds
// anything is errCMSExists, dimensions whose product would pass 63 bits are
// errCMSInitArgs, and dimensions this server will not allocate for one key
// errTooLargeForOneKey.
func (e *Engine) cmsCreate(key string, width, depth int64) error {
	if _, held := e.space.OwnerOf(key); held {
		return errCMSExists
	}
	if width < 1 || depth < 1 || width > math.MaxInt64/depth {
		return errCMSInitArgs
	}
	// RedisBloom allocates what it is asked for; this server bounds what one
	// key may hold, and four bytes a counter is checked against that before
	// anything is made.
	if width*depth > maxStructureBytes/4 {
		return errTooLargeForOneKey
	}
	w, d := uint32(width), uint32(depth)
	if err := e.affordable(data_structure.CMSMemUsageFor(w, d)); err != nil {
		return err
	}
	e.cmsStore.Put(key, data_structure.CreateCMS(w, d))
	return nil
}

// opCMSInitByDim creates a sketch of width by depth counters at key:
// CMS.INITBYDIM. text is the width and depth as a client wrote them.
func (e *Engine) opCMSInitByDim(key string, width, depth int64, text []string) error {
	if err := e.cmsCreate(key, width, depth); err != nil {
		return err
	}
	if text == nil {
		text = []string{strconv.FormatInt(width, 10), strconv.FormatInt(depth, 10)}
	}
	e.aofRecordKeyArgs("CMS.INITBYDIM", key, text)
	return nil
}

// opCMSInitByProb creates a sketch at key sized so an estimate is within
// errRate of the total count with probability 1 - prob, as RedisBloom sizes
// it (CalcCMSDim): CMS.INITBYPROB. text is the two rates as a client wrote
// them.
func (e *Engine) opCMSInitByProb(key string, errRate, prob float64, text []string) error {
	width, depth := data_structure.CalcCMSDim(errRate, prob)
	if width == 0 {
		// Dimensions past 32 bits are past what one key may hold.
		if _, held := e.space.OwnerOf(key); held {
			return errCMSExists
		}
		return errTooLargeForOneKey
	}
	if err := e.cmsCreate(key, int64(width), int64(depth)); err != nil {
		return err
	}
	if text == nil {
		text = formatRates(errRate, prob)
	}
	e.aofRecordKeyArgs("CMS.INITBYPROB", key, text)
	return nil
}

// opCMSIncrBy adds by[i] to items[i]'s counters in the sketch key holds and
// hands each its estimate after it, the maximum, 2^32 - 1, where a counter
// overflowed: CMS.INCRBY. A key that holds no sketch is errCMSMissing. text
// is the items and increments as a client wrote them, item, increment, item,
// increment. RedisBloom replicates every CMS.INCRBY it runs, so every one is
// recorded.
func (e *Engine) opCMSIncrBy(key string, items []string, by []uint64, text []string, each func(estimate uint32)) error {
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
			text = append(text, item, strconv.FormatUint(by[i], 10))
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

// cmsInfo is what CMS.INFO reports of a sketch.
type cmsInfo struct {
	width, depth uint32
	count        uint64
}

// opCMSInfo describes the sketch key holds: CMS.INFO. A key that holds no
// sketch is errCMSMissing; one of another type the type check refuses first.
func (e *Engine) opCMSInfo(key string) (cmsInfo, error) {
	// Peek rather than Get: reporting on a key is not using it.
	cms, ok := e.cmsStore.Peek(key)
	if !ok {
		return cmsInfo{}, errCMSMissing
	}
	return cmsInfo{width: cms.Width(), depth: cms.Depth(), count: cms.TotalCount()}, nil
}

// cmsMergeRequest is a CMS.MERGE: its sources, and the weight of each.
type cmsMergeRequest struct {
	sources []string
	// weight is the i-th source's weight, read as RedisBloom reads it, in
	// turn with the source, so that a malformed weight is refused where
	// RedisBloom refuses it, after the sources before it; nil is a weight of
	// 1 for every source.
	weight func(i int) (int64, error)
	// text is the arguments after the destination as the client wrote them,
	// which the record repeats; nil for a Go caller.
	text []string
}

// opCMSMerge sets the sketch dest holds to the weighted sum of the sketches
// the sources hold, as RedisBloom's CMSketch_Merge does: dest is the sum's,
// its own counters taking part only as a source. Each source is opened in
// turn, after its weight is read: a missing one is errCMSMissing, one of
// another type errWrongType, and one of other dimensions errCMSDimensions; a
// sum that will not fit is data_structure.ErrCMSMergeOverflow. Each source
// is a keyspace hit or a miss, as RedisBloom opens it to read.
func (e *Engine) opCMSMerge(dest string, req cmsMergeRequest) error {
	d, ok := e.cmsFor(dest)
	if !ok {
		return errCMSMissing
	}
	sources := make([]*data_structure.CMS, len(req.sources))
	weights := make([]int64, len(req.sources))
	for i, key := range req.sources {
		weights[i] = 1
		if req.weight != nil {
			w, err := req.weight(i)
			if err != nil {
				return err
			}
			weights[i] = w
		}
		src, held := e.cmsFor(key)
		if !held {
			other, any := e.space.OwnerOf(key)
			e.noteLookup(any)
			if any && other.KeyspaceName() != e.cmsStore.KeyspaceName() {
				return errWrongType
			}
			return errCMSMissing
		}
		e.noteLookup(true)
		if src.Width() != d.Width() || src.Depth() != d.Depth() {
			return errCMSDimensions
		}
		sources[i] = src
	}
	if err := d.MergeFrom(sources, weights); err != nil {
		return err
	}
	text := req.text
	if text == nil {
		text = append(text, strconv.Itoa(len(req.sources)))
		text = append(text, req.sources...)
		text = append(text, "WEIGHTS")
		for _, w := range weights {
			text = append(text, strconv.FormatInt(w, 10))
		}
	}
	e.aofRecordKeyArgs("CMS.MERGE", dest, text)
	return nil
}
