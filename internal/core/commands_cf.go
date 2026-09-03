package core

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/brandopakel/keel/internal/constant"
	"github.com/brandopakel/keel/internal/data_structure"
)

// Cuckoo filter commands, following the shape RedisBloom uses.
//
// The reason to pick one over the Bloom filter next door is deletion: a Bloom
// filter shares bits between items, so clearing them for one item would erase
// evidence of others, and it has no way to represent an item being present
// twice. A cuckoo filter stores a separate fingerprint per insertion, so both
// fall out naturally.

func cmdCFRESERVE(args []string) []byte {
	if len(args) != 2 {
		return Encode(errors.New("(error) ERR wrong number of arguments for 'CF.RESERVE' command"), false)
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
		return Encode(errors.New("(error) ERR wrong number of arguments for 'CF.ADD' command"), false)
	}
	if cfFor(args[0]).Insert(args[1]) {
		return constant.RespOne
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
		return Encode(errors.New("(error) ERR wrong number of arguments for 'CF.ADDNX' command"), false)
	}
	cf := cfFor(args[0])
	if cf.Lookup(args[1]) {
		return constant.RespZero
	}
	if cf.Insert(args[1]) {
		return constant.RespOne
	}
	return Encode(errors.New("CF: filter is full"), false)
}

func cmdCFEXISTS(args []string) []byte {
	if len(args) != 2 {
		return Encode(errors.New("(error) ERR wrong number of arguments for 'CF.EXISTS' command"), false)
	}
	cf, exist := cfStore.Get(args[0])
	if !exist || !cf.Lookup(args[1]) {
		return constant.RespZero
	}
	return constant.RespOne
}

func cmdCFMEXISTS(args []string) []byte {
	if len(args) < 2 {
		return Encode(errors.New("(error) ERR wrong number of arguments for 'CF.MEXISTS' command"), false)
	}
	cf, exist := cfStore.Get(args[0])
	var res []string
	for _, item := range args[1:] {
		if exist && cf.Lookup(item) {
			res = append(res, "1")
		} else {
			res = append(res, "0")
		}
	}
	return Encode(res, false)
}

func cmdCFDEL(args []string) []byte {
	if len(args) != 2 {
		return Encode(errors.New("(error) ERR wrong number of arguments for 'CF.DEL' command"), false)
	}
	cf, exist := cfStore.Get(args[0])
	if !exist || !cf.Delete(args[1]) {
		return constant.RespZero
	}
	return constant.RespOne
}

func cmdCFCOUNT(args []string) []byte {
	if len(args) != 2 {
		return Encode(errors.New("(error) ERR wrong number of arguments for 'CF.COUNT' command"), false)
	}
	cf, exist := cfStore.Get(args[0])
	if !exist {
		return constant.RespZero
	}
	return Encode(int64(cf.Count(args[1])), false)
}

func cmdCFINFO(args []string) []byte {
	if len(args) != 1 {
		return Encode(errors.New("(error) ERR wrong number of arguments for 'CF.INFO' command"), false)
	}
	key := args[0]
	cf, exist := cfStore.Peek(key)
	if !exist {
		return Encode(errors.New(fmt.Sprintf("Cuckoo filter with key '%s' does not exist", key)), false)
	}
	res := []string{
		"Capacity", fmt.Sprintf("%d", cf.Capacity()),
		"Size", fmt.Sprintf("%d", cf.MemUsage()),
		"Number of buckets", fmt.Sprintf("%d", cf.NumBuckets()),
		"Bucket size", fmt.Sprintf("%d", data_structure.CuckooBucketSize),
		"Max iterations", fmt.Sprintf("%d", data_structure.CuckooMaxKicks),
		"Number of items inserted", fmt.Sprintf("%d", cf.Inserted()),
		"Number of items deleted", fmt.Sprintf("%d", cf.Deleted()),
	}
	return Encode(res, false)
}
