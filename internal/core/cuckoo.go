package core

import (
	"strconv"

	"github.com/brandopakel/keel/internal/data_structure"
)

// Typed operations: cuckoo filters (phase 4 of docs/embedding-plan.md), with
// the semantics of RedisBloom 8.10.1's CF.* family (redisbloom.go). Like the
// Bloom filters' (bloom.go), each checks its key's type itself, where
// RedisBloom does, and each write stages its record, the command as
// RedisBloom replicates it, verbatim.

// cfFor returns the filter at key, creating a default-sized one if absent.
func (e *Engine) cfFor(key string) *data_structure.CuckooFilter {
	cf, exist := e.cfStore.Get(key)
	if !exist {
		cf = data_structure.CreateCuckooFilter(data_structure.CfDefaultCapacity)
		e.cfStore.Put(key, cf)
	}
	return cf
}

// cfForRead is the filter a read asks about: RedisBloom's CF.EXISTS,
// CF.MEXISTS and CF.COUNT answer no, and 0, for a key that holds anything else.
func (e *Engine) cfForRead(key string) (*data_structure.CuckooFilter, bool) {
	cf, status := filterRead(e, e.cfStore, key, false)
	return cf, status == filterHeld
}

// opCFReserve creates a filter of capacity at key: CF.RESERVE. A key that
// holds a filter is errFilterExists, one of another type errWrongType, and a
// geometry this server's filters do not have (foreign) errCFGeometry. text
// is the reservation's arguments after the key as the client wrote them.
func (e *Engine) opCFReserve(key string, capacity uint64, foreign bool, text []string) error {
	switch e.filterKeyStatus(key, e.cfStore) {
	case filterHeld:
		return errFilterExists
	case filterOtherType:
		return errWrongType
	}
	if foreign {
		return errCFGeometry
	}
	if capacity > maxStructureBytes/4 {
		return errTooLargeForOneKey
	}
	if err := e.affordable(data_structure.CuckooBytesFor(capacity)); err != nil {
		return err
	}
	e.cfStore.Put(key, data_structure.CreateCuckooFilter(capacity))
	if text == nil {
		text = []string{strconv.FormatUint(capacity, 10)}
	}
	e.aofRecordKeyArgs("CF.RESERVE", key, text)
	return nil
}

// opCFAdd adds item to the filter key holds, creating a default one: CF.ADD.
// A key of another type is errWrongType. A filter too full to take another
// fingerprint is errCFFull, and records nothing: unlike a Bloom filter,
// which degrades by growing less accurate, a cuckoo filter refuses.
func (e *Engine) opCFAdd(key, item string) error {
	if e.filterKeyStatus(key, e.cfStore) == filterOtherType {
		return errWrongType
	}
	if !e.cfFor(key).Insert(item) {
		return errCFFull
	}
	e.aofRecord("CF.ADD", key, item)
	return nil
}

// opCFAddNX adds item only if it appears to be absent, and reports whether it
// added it: CF.ADDNX. "Appears" is doing real work: the check is a filter
// lookup, so a false positive means a genuinely new item is reported as
// already present and is not added. A key of another type is errWrongType,
// and a full filter errCFFull, which records nothing.
//
// An item that appears present is recorded too, as the log has always held
// it, though nothing changed.
func (e *Engine) opCFAddNX(key, item string) (bool, error) {
	if e.filterKeyStatus(key, e.cfStore) == filterOtherType {
		return false, errWrongType
	}
	cf := e.cfFor(key)
	if cf.Lookup(item) {
		e.aofRecord("CF.ADDNX", key, item)
		return false, nil
	}
	if !cf.Insert(item) {
		return false, errCFFull
	}
	e.aofRecord("CF.ADDNX", key, item)
	return true, nil
}

// opCFExists reports whether each of items may be in the filter key holds:
// CF.EXISTS and CF.MEXISTS. A key that holds anything else answers no.
func (e *Engine) opCFExists(key string, items []string, each func(exists bool)) {
	cf, exist := e.cfForRead(key)
	for _, item := range items {
		each(exist && cf.Lookup(item))
	}
}

// opCFDel removes one copy of item from the filter key holds, and reports
// whether there was one: CF.DEL. A key that holds no cuckoo filter -
// nothing, or something else - is RedisBloom's errCFDelNotFound; replayed
// from a log, a missing key answers false, as the build before answered it.
//
// It is recorded whether it found the item or not, as the log has always
// held it.
func (e *Engine) opCFDel(key, item string) (bool, error) {
	cf, status := filterLookup(e, e.cfStore, key, false)
	switch status {
	case filterMissing:
		if e.replayingFilterLog() {
			return false, nil
		}
		return false, errCFDelNotFound
	case filterOtherType:
		return false, errCFDelNotFound
	}
	deleted := cf.Delete(item)
	e.aofRecord("CF.DEL", key, item)
	return deleted, nil
}

// opCFCount is how many times item may be in the filter key holds, zero for
// a key that holds anything else: CF.COUNT.
func (e *Engine) opCFCount(key, item string) int64 {
	cf, exist := e.cfForRead(key)
	if !exist {
		return 0
	}
	return int64(cf.Count(item))
}

// cfInfo is what CF.INFO reports of a filter.
type cfInfo struct {
	size, buckets, inserted, deleted uint64
}

// opCFInfo describes the filter key holds: CF.INFO. A key that holds nothing
// is errFilterNotFound, and one of another type errWrongType.
func (e *Engine) opCFInfo(key string) (cfInfo, error) {
	cf, status := filterRead(e, e.cfStore, key, true)
	switch status {
	case filterMissing:
		return cfInfo{}, errFilterNotFound
	case filterOtherType:
		return cfInfo{}, errWrongType
	}
	return cfInfo{size: cf.MemUsage(), buckets: uint64(cf.NumBuckets()), inserted: uint64(cf.Size()),
		deleted: uint64(cf.Deleted())}, nil
}

// cfInsertRequest is a CF.INSERT's or CF.INSERTNX's filter capacity, should
// it create one, and its items.
type cfInsertRequest struct {
	capacity uint64
	noCreate bool
	// nx adds an item only if it appears absent: CF.INSERTNX.
	nx    bool
	items []string
	// text is the arguments after the key as the client wrote them, which the
	// record repeats; nil for a Go caller.
	text []string
}

// cfInsertResult is what one item of a CF.INSERT or CF.INSERTNX came to.
type cfInsertResult uint8

const (
	cfInserted cfInsertResult = iota
	// cfExists is an item CF.INSERTNX found present already.
	cfExists
	// cfNoSpace is an item a full filter could not take.
	cfNoSpace
)

// opCFInsert adds each of req's items to the filter key holds, creating one
// of req's capacity for a key that holds none, and hands each what it came
// to: CF.INSERT, and CF.INSERTNX with nx. A full filter answers the items it
// cannot take and goes on, as RedisBloom's cfInsertCommon does. A key that
// holds none with NOCREATE is errFilterNotFound, and one of another type
// errWrongType. RedisBloom replicates every one that reaches its items, so
// every one is recorded.
func (e *Engine) opCFInsert(key string, req cfInsertRequest, each func(cfInsertResult)) error {
	cf, status := filterLookup(e, e.cfStore, key, false)
	switch status {
	case filterOtherType:
		return errWrongType
	case filterMissing:
		if req.noCreate {
			return errFilterNotFound
		}
		if req.capacity > maxStructureBytes/4 {
			return errTooLargeForOneKey
		}
		if err := e.affordable(data_structure.CuckooBytesFor(req.capacity)); err != nil {
			return err
		}
		cf = data_structure.CreateCuckooFilter(req.capacity)
		e.cfStore.Put(key, cf)
	}
	for _, item := range req.items {
		switch {
		case req.nx && cf.Lookup(item):
			each(cfExists)
		case cf.Insert(item):
			each(cfInserted)
		default:
			each(cfNoSpace)
		}
	}
	name, text := "CF.INSERT", req.text
	if req.nx {
		name = "CF.INSERTNX"
	}
	if text == nil {
		text = []string{"CAPACITY", strconv.FormatUint(req.capacity, 10)}
		if req.noCreate {
			text = append(text, "NOCREATE")
		}
		text = append(append(text, "ITEMS"), req.items...)
	}
	e.aofRecordKeyArgs(name, key, text)
	return nil
}

// opCFCompact compacts the filter key holds, as RedisBloom's CF.COMPACT does:
// a filter of one table has no later table to empty, and compaction resets
// its count of deletions. A key that holds no cuckoo filter is
// errCFCompactNotFound. RedisBloom replicates it, so it is recorded.
func (e *Engine) opCFCompact(key string) error {
	cf, status := filterLookup(e, e.cfStore, key, false)
	if status != filterHeld {
		return errCFCompactNotFound
	}
	cf.Compact()
	e.aofRecord("CF.COMPACT", key)
	return nil
}
