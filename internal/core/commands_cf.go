package core

import (
	"errors"
	"fmt"
	"strconv"

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

// cmdCFRESERVE implements CF.RESERVE key capacity [BUCKETSIZE n]
// [MAXITERATIONS n] [EXPANSION n]; see opCFReserve.
func (e *Engine) cmdCFRESERVE(args []string) []byte {
	e.recordsItself()
	if len(args) < 2 || len(args)%2 == 1 {
		return e.encode(wrongArguments("CF.RESERVE"), false)
	}
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
	r := e.reply()
	if err := e.opCFReserve(args[0], capacity, foreign, args[1:]); err != nil {
		return r.Error(err).Bytes()
	}
	return r.Status("OK").Bytes()
}

func (e *Engine) cmdCFADD(args []string) []byte {
	e.recordsItself()
	if len(args) != 2 {
		return e.encode(wrongArguments("CF.ADD"), false)
	}
	r := e.reply()
	if err := e.opCFAdd(args[0], args[1]); err != nil {
		return r.Error(err).Bytes()
	}
	return r.Bool(true).Bytes()
}

// cmdCFADDNX adds only if the item appears to be absent; see opCFAddNX.
func (e *Engine) cmdCFADDNX(args []string) []byte {
	e.recordsItself()
	if len(args) != 2 {
		return e.encode(wrongArguments("CF.ADDNX"), false)
	}
	r := e.reply()
	added, err := e.opCFAddNX(args[0], args[1])
	if err != nil {
		return r.Error(err).Bytes()
	}
	return r.Bool(added).Bytes()
}

func (e *Engine) cmdCFEXISTS(args []string) []byte {
	if len(args) != 2 {
		return e.encode(wrongArguments("CF.EXISTS"), false)
	}
	r := e.reply()
	e.opCFExists(args[0], args[1:], func(exists bool) { r.Bool(exists) })
	return r.Bytes()
}

func (e *Engine) cmdCFMEXISTS(args []string) []byte {
	if len(args) < 2 {
		return e.encode(wrongArguments("CF.MEXISTS"), false)
	}
	r := e.reply()
	r.Array(len(args) - 1)
	e.opCFExists(args[0], args[1:], func(exists bool) { r.Bool(exists) })
	return r.Bytes()
}

// cmdCFDEL removes one copy of an item; see opCFDel.
func (e *Engine) cmdCFDEL(args []string) []byte {
	e.recordsItself()
	if len(args) != 2 {
		return e.encode(wrongArguments("CF.DEL"), false)
	}
	r := e.reply()
	deleted, err := e.opCFDel(args[0], args[1])
	if err != nil {
		return r.Error(err).Bytes()
	}
	return r.Bool(deleted).Bytes()
}

func (e *Engine) cmdCFCOUNT(args []string) []byte {
	if len(args) != 2 {
		return e.encode(wrongArguments("CF.COUNT"), false)
	}
	r := e.reply()
	return r.Int(e.opCFCount(args[0], args[1])).Bytes()
}

// cmdCFINFO implements CF.INFO key with RedisBloom's fields, in its order.
// "Number of items inserted" is, as RedisBloom counts it, the items the filter
// holds now: an insert adds one and a delete takes one away.
func (e *Engine) cmdCFINFO(args []string) []byte {
	if len(args) != 1 {
		return e.encode(wrongArguments("CF.INFO"), false)
	}
	info, err := e.opCFInfo(args[0])
	if err != nil {
		return e.encode(err, false)
	}
	return e.infoReply([]infoEntry{
		{"Size", int64(info.size)},
		{"Number of buckets", int64(info.buckets)},
		{"Number of filters", int64(1)},
		{"Number of items inserted", int64(info.inserted)},
		{"Number of items deleted", int64(info.deleted)},
		{"Bucket size", int64(data_structure.CuckooBucketSize)},
		{"Expansion rate", int64(0)},
		{"Max iterations", int64(data_structure.CuckooMaxKicks)},
	})
}
