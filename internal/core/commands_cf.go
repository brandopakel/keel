package core

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/brandopakel/keel/internal/constant"
	"github.com/brandopakel/keel/internal/data_structure"
)

// Cuckoo filter commands, with the names, replies and refusals of RedisBloom
// 8.10.1's CF.* family - see redisbloom.go - in RESP2 and RESP3: booleans in
// RESP3 and integers in RESP2 for yes-or-no answers, and a map for CF.INFO.
//
// The reason to pick one over the Bloom filter next door is deletion: a Bloom
// filter shares bits between items, so clearing them for one item would erase
// evidence of others, and it has no way to represent an item being present
// twice. A cuckoo filter stores a separate fingerprint per insertion, so both
// fall out naturally.
//
// This server's cuckoo filters have one geometry: buckets of four 16-bit
// fingerprints, up to 500 evictions per insert, and no growth - a full filter
// refuses an item. In RedisBloom's terms that is BUCKETSIZE 4,
// MAXITERATIONS 500 and EXPANSION 0, and CF.INFO reports it so. CF.RESERVE
// reads all three options as RedisBloom does, with its refusals, and refuses a
// value other than those rather than build a filter that is not what was
// asked for.

var (
	errCFBadCapacity   = errors.New("Bad capacity")
	errCFCapacityRange = errors.New("Capacity must be in the range [2 * BUCKETSIZE, 1073741824]")
	errCFFull          = errors.New("Filter is full")
	errCFDelNotFound   = errors.New("Not found")
	errCFGeometry      = errors.New("ERR this server's cuckoo filters have BUCKETSIZE 4, MAXITERATIONS 500 and EXPANSION 0 only")
)

// cfOption is one of CF.RESERVE's options: RedisBloom's default and range for
// it, and the one value this server's filters have.
type cfOption struct {
	name          string
	def, min, max int64
	native        int64
}

// The options in the order RedisBloom reads them, which is the order a
// reservation with more than one bad option is refused in.
var cfOptions = []cfOption{
	{"MAXITERATIONS", 20, 1, 65535, data_structure.CuckooMaxKicks},
	{"BUCKETSIZE", 2, 1, 255, data_structure.CuckooBucketSize},
	{"EXPANSION", 1, 0, 32768, 0},
}

// cfMaxCapacity is the top of RedisBloom's cf-initial-size range.
const cfMaxCapacity = 1 << 30

// parseCFReserve reads CF.RESERVE key capacity [BUCKETSIZE n]
// [MAXITERATIONS n] [EXPANSION n] as RedisBloom does: the capacity, then each
// option, found anywhere among the arguments, then the capacity against the
// bucket size. An argument it does not know is ignored, as RedisBloom ignores
// it. foreign is whether an option names a geometry this server's filters do
// not have.
func parseCFReserve(args []string) (capacity uint64, foreign bool, err error) {
	c, ok := redisInteger(args[1])
	if !ok {
		return 0, false, errCFBadCapacity
	}
	bucketSize := int64(0)
	for _, o := range cfOptions {
		value := o.def
		if at := moduleArgIndex(o.name, args); at >= 0 {
			if at+1 < len(args) {
				value, ok = redisInteger(args[at+1])
			} else {
				ok = false
			}
			if !ok {
				return 0, false, fmt.Errorf("Couldn't parse %s", o.name)
			}
			if value < o.min || value > o.max {
				return 0, false, fmt.Errorf("%s: value must be in the range [%d, %d]", o.name, o.min, o.max)
			}
			foreign = foreign || value != o.native
		}
		if o.name == "BUCKETSIZE" {
			bucketSize = value
		}
	}
	if bucketSize*2 > c || c > cfMaxCapacity {
		return 0, false, errCFCapacityRange
	}
	return uint64(c), foreign, nil
}

// parseLegacyCFReserve is CF.RESERVE as the build before RedisBloom parity
// read it: a key and a capacity of at least 1, read by strconv. It reads only
// what a log can hold - see replayingFilterLog.
func parseLegacyCFReserve(args []string) (uint64, bool) {
	if len(args) != 2 {
		return 0, false
	}
	capacity, err := strconv.ParseUint(args[1], 10, 64)
	return capacity, err == nil && capacity >= 1
}

func (e *Engine) cmdCFRESERVE(args []string) []byte {
	if len(args) < 2 || len(args)%2 == 1 {
		return e.encode(wrongArguments("CF.RESERVE"), false)
	}
	key := args[0]
	// A form the earlier build accepted, replayed, means what it meant to
	// that build - see replayingFilterLog. That includes a key named after an
	// option, CF.RESERVE expansion 1000, which RedisBloom reads as the option.
	// Every form both builds accept from a client, they read alike, so a log
	// this build writes replays the same way.
	capacity, legacyForm := parseLegacyCFReserve(args)
	foreign := false
	if !legacyForm || !e.replayingFilterLog() {
		var err error
		if capacity, foreign, err = parseCFReserve(args); err != nil {
			return e.encode(err, false)
		}
	}
	switch e.filterKeyStatus(key, e.cfStore) {
	case filterHeld:
		return e.encode(errFilterExists, false)
	case filterOtherType:
		return e.encode(errWrongType, false)
	}
	if foreign {
		return e.encode(errCFGeometry, false)
	}
	if capacity > maxStructureBytes/4 {
		return e.encode(errTooLargeForOneKey, false)
	}
	cfBytes := data_structure.CuckooBytesFor(capacity)
	if err := e.affordable(cfBytes); err != nil {
		return e.encode(err, false)
	}
	e.cfStore.Put(key, data_structure.CreateCuckooFilter(capacity))
	return constant.RespOk
}

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
	if e.filterKeyRead(key, e.cfStore) != filterHeld {
		return nil, false
	}
	return e.cfStore.Get(key)
}

func (e *Engine) cmdCFADD(args []string) []byte {
	if len(args) != 2 {
		return e.encode(wrongArguments("CF.ADD"), false)
	}
	if e.filterKeyStatus(args[0], e.cfStore) == filterOtherType {
		return e.encode(errWrongType, false)
	}
	if e.cfFor(args[0]).Insert(args[1]) {
		return e.boolReply(true)
	}
	// The filter is too full to take another fingerprint. Unlike a Bloom
	// filter, which degrades by growing less accurate, a cuckoo filter refuses.
	return e.encode(errCFFull, false)
}

// cmdCFADDNX adds only if the item appears to be absent.
//
// "Appears" is doing real work: the check is a filter lookup, so a false
// positive means a genuinely new item is reported as already present and is not
// added. CF.ADD has no such failure mode.
func (e *Engine) cmdCFADDNX(args []string) []byte {
	if len(args) != 2 {
		return e.encode(wrongArguments("CF.ADDNX"), false)
	}
	if e.filterKeyStatus(args[0], e.cfStore) == filterOtherType {
		return e.encode(errWrongType, false)
	}
	cf := e.cfFor(args[0])
	if cf.Lookup(args[1]) {
		return e.boolReply(false)
	}
	if cf.Insert(args[1]) {
		return e.boolReply(true)
	}
	return e.encode(errCFFull, false)
}

func (e *Engine) cmdCFEXISTS(args []string) []byte {
	if len(args) != 2 {
		return e.encode(wrongArguments("CF.EXISTS"), false)
	}
	cf, exist := e.cfForRead(args[0])
	return e.boolReply(exist && cf.Lookup(args[1]))
}

func (e *Engine) cmdCFMEXISTS(args []string) []byte {
	if len(args) < 2 {
		return e.encode(wrongArguments("CF.MEXISTS"), false)
	}
	cf, exist := e.cfForRead(args[0])
	res := make([]interface{}, 0, len(args)-1)
	for _, item := range args[1:] {
		res = append(res, ReplyBool(exist && cf.Lookup(item)))
	}
	return e.encode(res, false)
}

// cmdCFDEL removes one copy of an item. A key that holds no cuckoo filter -
// nothing, or something else - is RedisBloom's "Not found".
func (e *Engine) cmdCFDEL(args []string) []byte {
	if len(args) != 2 {
		return e.encode(wrongArguments("CF.DEL"), false)
	}
	switch e.filterKeyStatus(args[0], e.cfStore) {
	case filterMissing:
		if e.replayingFilterLog() {
			// The build before answered 0 here, so its log records this.
			return constant.RespZero
		}
		return e.encode(errCFDelNotFound, false)
	case filterOtherType:
		return e.encode(errCFDelNotFound, false)
	}
	cf, _ := e.cfStore.Get(args[0])
	return e.boolReply(cf.Delete(args[1]))
}

func (e *Engine) cmdCFCOUNT(args []string) []byte {
	if len(args) != 2 {
		return e.encode(wrongArguments("CF.COUNT"), false)
	}
	cf, exist := e.cfForRead(args[0])
	if !exist {
		return constant.RespZero
	}
	return e.encode(int64(cf.Count(args[1])), false)
}

// cmdCFINFO implements CF.INFO key with RedisBloom's fields, in its order.
// "Number of items inserted" is, as RedisBloom counts it, the items the filter
// holds now: an insert adds one and a delete takes one away.
func (e *Engine) cmdCFINFO(args []string) []byte {
	if len(args) != 1 {
		return e.encode(wrongArguments("CF.INFO"), false)
	}
	key := args[0]
	switch e.filterKeyRead(key, e.cfStore) {
	case filterMissing:
		return e.encode(errFilterNotFound, false)
	case filterOtherType:
		return e.encode(errWrongType, false)
	}
	cf, _ := e.cfStore.Peek(key)
	return e.infoReply([]infoEntry{
		{"Size", int64(cf.MemUsage())},
		{"Number of buckets", int64(cf.NumBuckets())},
		{"Number of filters", int64(1)},
		{"Number of items inserted", int64(cf.Size())},
		{"Number of items deleted", int64(cf.Deleted())},
		{"Bucket size", int64(data_structure.CuckooBucketSize)},
		{"Expansion rate", int64(0)},
		{"Max iterations", int64(data_structure.CuckooMaxKicks)},
	})
}
