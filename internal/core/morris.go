package core

import (
	"errors"
	"strconv"

	"github.com/brandopakel/keel/internal/data_structure"
)

// Typed operations: Morris counters (phase 4 of docs/embedding-plan.md),
// Keel's own sketch, in the shape of the Count-Min sketch's operations next
// door (cms.go): each stages its record, the command as a client sent it,
// its numbers as the client wrote them, handed in as text; a Go caller's
// record is written from the values.

var (
	errMorrisExists    = errors.New("MORRIS: key already exists")
	errMorrisMissing   = errors.New("MORRIS: key does not exist")
	errMorrisErrRate   = errors.New("MORRIS: invalid overestimation value")
	errMorrisProb      = errors.New("MORRIS: invalid prob value")
	errMorrisIncrement = errors.New("ERR increment must be a non negative integer")
)

// morrisCreate stores a fresh table of width by depth counters at key, once
// its memory is affordable.
func (e *Engine) morrisCreate(key string, width, depth uint32) error {
	if err := e.affordable(64 + uint64(width)*uint64(depth)); err != nil {
		return err
	}
	e.morrisStore.Put(key, data_structure.CreateMorris(width, depth))
	return nil
}

// opMorrisInitByDim creates a table of width by depth counters at key:
// MORRIS.INITBYDIM. A key already taken is errMorrisExists. text is the width
// and depth as a client wrote them.
func (e *Engine) opMorrisInitByDim(key string, width, depth uint32, text []string) error {
	if e.morrisStore.Exists(key) {
		return errMorrisExists
	}
	if err := e.morrisCreate(key, width, depth); err != nil {
		return err
	}
	if text == nil {
		text = formatUint32s(width, depth)
	}
	e.aofRecordKeyArgs("MORRIS.INITBYDIM", key, text)
	return nil
}

// opMorrisInitByProb creates a table at key sized so the hashing's error is
// within errRate of the total count with probability 1 - prob:
// MORRIS.INITBYPROB. Those bound the error the hashing contributes; the
// counters contribute their own on top, which no table dimension can reduce,
// and MORRIS.INFO reports it so the two are not confused for each other.
// text is the two rates as a client wrote them.
func (e *Engine) opMorrisInitByProb(key string, errRate, prob float64, text []string) error {
	if e.morrisStore.Exists(key) {
		return errMorrisExists
	}
	w, d := data_structure.CalcMorrisDim(errRate, prob)
	if w == 0 || d == 0 {
		return errTooLargeForOneKey
	}
	if err := e.morrisCreate(key, w, d); err != nil {
		return err
	}
	if text == nil {
		text = formatRates(errRate, prob)
	}
	e.aofRecordKeyArgs("MORRIS.INITBYPROB", key, text)
	return nil
}

// opMorrisIncrBy adds by[i] to items[i]'s counters in the table key holds and
// hands each the estimate after it: MORRIS.INCRBY. A key that holds no table
// is errMorrisMissing. The table is a fixed size, so this cannot change what
// the key costs, and the store is not asked to measure it again. text is the
// items and increments as a client wrote them.
func (e *Engine) opMorrisIncrBy(key string, items []string, by []uint64, text []string, each func(estimate uint64)) error {
	m, exist := e.morrisStore.Get(key)
	if !exist {
		return errMorrisMissing
	}
	for i, item := range items {
		each(m.IncrBy(item, by[i]))
	}
	if text == nil {
		text = make([]string, 0, 2*len(items))
		for i, item := range items {
			text = append(text, item, strconv.FormatUint(by[i], 10))
		}
	}
	e.aofRecordKeyArgs("MORRIS.INCRBY", key, text)
	return nil
}

// opMorrisQuery hands each of items' estimates in the table key holds, in
// order: MORRIS.QUERY. A key that holds no table is errMorrisMissing.
func (e *Engine) opMorrisQuery(key string, items []string, each func(count uint64)) error {
	m, exist := e.morrisStore.Get(key)
	if !exist {
		return errMorrisMissing
	}
	for _, item := range items {
		each(m.Count(item))
	}
	return nil
}
