package core

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/brandopakel/keel/internal/constant"
	"github.com/brandopakel/keel/internal/data_structure"
)

// Cuckoo filter commands, following the shape RedisBloom uses, including its
// RESP3 one: booleans for yes-or-no answers and a map for CF.INFO.
//
// The reason to pick one over the Bloom filter next door is deletion: a Bloom
// filter shares bits between items, so clearing them for one item would erase
// evidence of others, and it has no way to represent an item being present
// twice. A cuckoo filter stores a separate fingerprint per insertion, so both
// fall out naturally.

func cmdCFRESERVE(args []string) []byte {
	if len(args) != 2 {
		return Encode(wrongArguments("CF.RESERVE"), false)
	}
	key := args[0]
	capacity, err := strconv.ParseUint(args[1], 10, 64)
	if err != nil {
		return Encode(errors.New(fmt.Sprintf("capacity must be an integer number %s", args[1])), false)
	}
	if capacity < 1 {
		return Encode(errors.New("CF: capacity must be at least 1"), false)
	}
	if cfStore.Exists(key) {
		return Encode(errors.New("CF: key already exists"), false)
	}
	if capacity > maxStructureBytes/4 {
		return Encode(errTooLargeForOneKey, false)
	}
	cfBytes := data_structure.CuckooBytesFor(capacity)
	if err := affordable(cfBytes); err != nil {
		return Encode(err, false)
	}
	cfStore.Put(key, data_structure.CreateCuckooFilter(capacity))
	return constant.RespOk
}

// cfFor returns the filter at key, creating a default-sized one if absent.
func cfFor(key string) *data_structure.CuckooFilter {
	cf, exist := cfStore.Get(key)
	if !exist {
		cf = data_structure.CreateCuckooFilter(data_structure.CfDefaultCapacity)
		cfStore.Put(key, cf)
	}
	return cf
}

func cmdCFADD(args []string) []byte {
	if len(args) != 2 {
		return Encode(wrongArguments("CF.ADD"), false)
	}
	if cfFor(args[0]).Insert(args[1]) {
		return boolReply(true)
	}
	// The filter is too full to take another fingerprint. Unlike a Bloom
	// filter, which degrades by growing less accurate, a cuckoo filter refuses.
	return Encode(errors.New("CF: filter is full"), false)
}

// cmdCFADDNX adds only if the item appears to be absent.
//
// "Appears" is doing real work: the check is a filter lookup, so a false
// positive means a genuinely new item is reported as already present and is not
// added. CF.ADD has no such failure mode.
func cmdCFADDNX(args []string) []byte {
	if len(args) != 2 {
		return Encode(wrongArguments("CF.ADDNX"), false)
	}
	cf := cfFor(args[0])
	if cf.Lookup(args[1]) {
		return boolReply(false)
	}
	if cf.Insert(args[1]) {
		return boolReply(true)
	}
	return Encode(errors.New("CF: filter is full"), false)
}

func cmdCFEXISTS(args []string) []byte {
	if len(args) != 2 {
		return Encode(wrongArguments("CF.EXISTS"), false)
	}
	cf, exist := cfStore.Get(args[0])
	return boolReply(exist && cf.Lookup(args[1]))
}

func cmdCFMEXISTS(args []string) []byte {
	if len(args) < 2 {
		return Encode(wrongArguments("CF.MEXISTS"), false)
	}
	cf, exist := cfStore.Get(args[0])
	res := make([]interface{}, 0, len(args)-1)
	for _, item := range args[1:] {
		res = append(res, replyTextBool(exist && cf.Lookup(item)))
	}
	return Encode(res, false)
}

func cmdCFDEL(args []string) []byte {
	if len(args) != 2 {
		return Encode(wrongArguments("CF.DEL"), false)
	}
	cf, exist := cfStore.Get(args[0])
	return boolReply(exist && cf.Delete(args[1]))
}

func cmdCFCOUNT(args []string) []byte {
	if len(args) != 2 {
		return Encode(wrongArguments("CF.COUNT"), false)
	}
	cf, exist := cfStore.Get(args[0])
	if !exist {
		return constant.RespZero
	}
	return Encode(int64(cf.Count(args[1])), false)
}

func cmdCFINFO(args []string) []byte {
	if len(args) != 1 {
		return Encode(wrongArguments("CF.INFO"), false)
	}
	key := args[0]
	cf, exist := cfStore.Peek(key)
	if !exist {
		return Encode(errors.New(fmt.Sprintf("Cuckoo filter with key '%s' does not exist", key)), false)
	}
	// RESP2 has always sent these numbers as strings here; RESP3 sends the
	// integers RedisBloom does.
	return infoReply([]infoEntry{
		{"Capacity", int64(cf.Capacity())},
		{"Size", int64(cf.MemUsage())},
		{"Number of buckets", int64(cf.NumBuckets())},
		{"Bucket size", int64(data_structure.CuckooBucketSize)},
		{"Max iterations", int64(data_structure.CuckooMaxKicks)},
		{"Number of items inserted", int64(cf.Inserted())},
		{"Number of items deleted", int64(cf.Deleted())},
	}, true)
}
