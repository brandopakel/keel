package core

import (
	"strconv"

	"github.com/brandopakel/keel/internal/data_structure"
)

// Typed operations: Bloom filters (phase 4 of docs/embedding-plan.md), with
// the semantics of RedisBloom 8.10.1's BF.* family (redisbloom.go).
//
// A filter command checks its key's type itself, where RedisBloom does
// (filterKeyStatus), so these operations do too. Each write stages its
// record: the command as RedisBloom replicates it, verbatim, its arguments
// as the client wrote them, handed in as text (see cms.go).

func (e *Engine) bloomFor(key string) (*data_structure.SBChain, bool) {
	return e.sbStore.Get(key)
}

// bloomForWrite returns the filter at key, creating one with the default sizing
// if there is none: adding to a filter that was never reserved is how most
// filters come to exist.
func (e *Engine) bloomForWrite(key string) *data_structure.SBChain {
	sb, ok := e.sbStore.Get(key)
	if !ok {
		sb = data_structure.CreateSBChain(data_structure.BfDefaultInitCapacity,
			data_structure.BfDefaultErrRate, data_structure.BfDefaultExpansion)
		e.sbStore.Put(key, sb)
	}
	return sb
}

// bloomForRead is the filter a read asks about: RedisBloom's BF.EXISTS and
// BF.MEXISTS answer no for a key that holds anything else.
func (e *Engine) bloomForRead(key string) (*data_structure.SBChain, bool) {
	if e.filterKeyRead(key, e.sbStore) != filterHeld {
		return nil, false
	}
	return e.bloomFor(key)
}

// opBFReserve creates the filter r describes at key: BF.RESERVE. A key that
// holds a filter is errFilterExists, and one of another type errWrongType.
// text is the reservation's arguments after the key as they are to be
// recorded: as the client wrote them, but for the one form this build reads
// differently from the build before (cmdBFRESERVE).
func (e *Engine) opBFReserve(key string, r bfReservation, text []string) error {
	switch e.filterKeyStatus(key, e.sbStore) {
	case filterHeld:
		return errFilterExists
	case filterOtherType:
		return errWrongType
	}
	// RedisBloom sizes the first filter of a chain that grows for half the
	// rate asked for, and cannot build one for a rate that halves to zero.
	// The earlier build could, so a log may hold one.
	if r.expansion != 0 && r.errorRate/2 == 0 && !e.replayingFilterLog() {
		return errBFCouldNotCreate
	}
	if err := e.affordable(data_structure.BloomBytesFor(r.capacity, r.errorRate)); err != nil {
		return err
	}
	e.sbStore.Put(key, data_structure.CreateSBChain(r.capacity, r.errorRate, r.expansion))
	if text == nil {
		text = []string{strconv.FormatFloat(r.errorRate, 'g', -1, 64), strconv.FormatUint(r.capacity, 10)}
		if r.expansion == 0 {
			text = append(text, "NONSCALING")
		} else {
			text = append(text, "EXPANSION", strconv.FormatUint(r.expansion, 10))
		}
	}
	e.aofRecordKeyArgs("BF.RESERVE", key, text)
	return nil
}

// opBFAdd adds item to the filter key holds, creating a default one, and
// reports whether it was new: BF.ADD. A key of another type is
// errWrongType, and a filter that cannot take the item its error, which
// records nothing.
func (e *Engine) opBFAdd(key, item string) (bool, error) {
	if e.filterKeyStatus(key, e.sbStore) == filterOtherType {
		return false, errWrongType
	}
	sb := e.bloomForWrite(key)
	added, err := sb.Add(item)
	// A scalable Bloom filter grows by adding filters as it fills, so its size
	// after this command is not the size the store recorded on Put.
	e.sbStore.Resize(key)
	if err != nil {
		return false, err
	}
	e.aofRecord("BF.ADD", key, item)
	return added, nil
}

// opBFMAdd adds each of items to the filter key holds, creating a default
// one, and hands each its answer as opBFAdd's: BF.MADD. A filter that does
// not grow and is full stops it: RedisBloom answers the items before that
// one and the refusal, and tries none after. A filter that could not grow to
// take an item answers that item with its error, and the items after it are
// tried in turn. A key of another type is errWrongType.
func (e *Engine) opBFMAdd(key string, items []string, each func(added bool, err error)) error {
	if e.filterKeyStatus(key, e.sbStore) == filterOtherType {
		return errWrongType
	}
	sb := e.bloomForWrite(key)
	for _, item := range items {
		added, err := sb.Add(item)
		each(added, err)
		if err == data_structure.ErrNonScalingFull {
			break
		}
	}
	e.sbStore.Resize(key)
	e.aofRecordKeyArgs("BF.MADD", key, items)
	return nil
}

// opBFExists reports whether item may be in the filter key holds: BF.EXISTS
// and each item of BF.MEXISTS. A key that holds anything else answers no.
func (e *Engine) opBFExists(key string, items []string, each func(exists bool)) {
	sb, ok := e.bloomForRead(key)
	for _, item := range items {
		each(ok && sb.Exists(item))
	}
}

// bfInfo is what BF.INFO reports of a filter.
type bfInfo struct {
	capacity, size, filters, items uint64
	// expansion is the growth rate; nonScaling says there is none.
	expansion  uint64
	nonScaling bool
}

// opBFInfo describes the filter key holds: BF.INFO. A key that holds nothing
// is errFilterNotFound, and one of another type errWrongType.
func (e *Engine) opBFInfo(key string) (bfInfo, error) {
	switch e.filterKeyRead(key, e.sbStore) {
	case filterMissing:
		return bfInfo{}, errFilterNotFound
	case filterOtherType:
		return bfInfo{}, errWrongType
	}
	// Peek rather than Get: reporting on a key is not using it.
	sb, _ := e.sbStore.Peek(key)
	return bfInfo{capacity: sb.Capacity(), size: sb.MemUsage(), filters: uint64(sb.Filters()),
		items: sb.Count(), expansion: uint64(sb.Expansion()), nonScaling: sb.NonScaling()}, nil
}

// bfInsertRequest is a BF.INSERT's filter, should it create one, and its
// items.
type bfInsertRequest struct {
	// r is the filter BF.INSERT creates for a key that holds none, unless
	// noCreate.
	r        bfReservation
	noCreate bool
	items    []string
	// text is the arguments after the key as the client wrote them, which the
	// record repeats; nil for a Go caller.
	text []string
}

// opBFInsert adds each of req's items to the filter key holds, creating the
// one req describes for a key that holds none, and hands each its answer as
// opBFMAdd's: BF.INSERT. A key that holds none with NOCREATE is
// errFilterNotFound, and one of another type errWrongType. RedisBloom
// replicates every BF.INSERT that reaches its items, so every one is
// recorded.
func (e *Engine) opBFInsert(key string, req bfInsertRequest, each func(added bool, err error)) error {
	switch e.filterKeyStatus(key, e.sbStore) {
	case filterOtherType:
		return errWrongType
	case filterMissing:
		if req.noCreate {
			return errFilterNotFound
		}
		r := req.r
		if r.expansion != 0 && r.errorRate/2 == 0 {
			return errBFCouldNotCreate
		}
		if err := e.affordable(data_structure.BloomBytesFor(r.capacity, r.errorRate)); err != nil {
			return err
		}
		sb := data_structure.CreateSBChain(r.capacity, r.errorRate, r.expansion)
		if sb == nil {
			return errBFCouldNotCreate
		}
		e.sbStore.Put(key, sb)
	}
	sb, _ := e.bloomFor(key)
	for _, item := range req.items {
		added, err := sb.Add(item)
		each(added, err)
		if err == data_structure.ErrNonScalingFull {
			break
		}
	}
	e.sbStore.Resize(key)
	text := req.text
	if text == nil {
		text = []string{"CAPACITY", strconv.FormatUint(req.r.capacity, 10),
			"ERROR", strconv.FormatFloat(req.r.errorRate, 'g', -1, 64)}
		if req.r.expansion == 0 {
			text = append(text, "NONSCALING")
		} else {
			text = append(text, "EXPANSION", strconv.FormatUint(req.r.expansion, 10))
		}
		if req.noCreate {
			text = append(text, "NOCREATE")
		}
		text = append(append(text, "ITEMS"), req.items...)
	}
	e.aofRecordKeyArgs("BF.INSERT", key, text)
	return nil
}

// opBFCard is how many items have been added to the filter key holds, zero
// for a key that holds none: BF.CARD. A key of another type is errWrongType.
func (e *Engine) opBFCard(key string) (int64, error) {
	switch e.filterKeyRead(key, e.sbStore) {
	case filterMissing:
		return 0, nil
	case filterOtherType:
		return 0, errWrongType
	}
	sb, _ := e.sbStore.Peek(key)
	return int64(sb.Count()), nil
}
