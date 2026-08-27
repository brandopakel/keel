package core

import (
	"errors"

	"memkv/internal/constant"
	"memkv/internal/data_structure"
)

// HyperLogLog commands, following Redis semantics.
//
// Note that keys live in a per-type map here, as they do for the Bloom filter
// and Count-Min sketch, so a name used as a HyperLogLog and as a string are
// separate keys rather than a type conflict.

func cmdPFADD(args []string) []byte {
	if len(args) < 1 {
		return Encode(errors.New("(error) ERR wrong number of arguments for 'PFADD' command"), false)
	}

	key := args[0]
	hll, exist := hllStore[key]
	// Creating the key is itself a change, which is why PFADD with no elements
	// still reports 1 the first time it is called.
	changed := !exist
	if !exist {
		hll = data_structure.CreateHLL()
		hllStore[key] = hll
	}

	for _, item := range args[1:] {
		if hll.Add(item) {
			changed = true
		}
	}

	if changed {
		return constant.RespOne
	}
	return constant.RespZero
}

func cmdPFCOUNT(args []string) []byte {
	if len(args) < 1 {
		return Encode(errors.New("(error) ERR wrong number of arguments for 'PFCOUNT' command"), false)
	}

	if len(args) == 1 {
		hll, exist := hllStore[args[0]]
		if !exist {
			return constant.RespZero
		}
		return Encode(int64(hll.Count()), false)
	}

	// Several keys means the cardinality of their union. It is computed into a
	// temporary rather than by merging into the first key, because PFCOUNT is a
	// read: the stored sketches must come out unchanged.
	union := data_structure.CreateHLL()
	for _, key := range args {
		if hll, exist := hllStore[key]; exist {
			union.Merge(hll)
		}
	}
	return Encode(int64(union.Count()), false)
}

func cmdPFMERGE(args []string) []byte {
	if len(args) < 1 {
		return Encode(errors.New("(error) ERR wrong number of arguments for 'PFMERGE' command"), false)
	}

	dest := args[0]
	merged := data_structure.CreateHLL()
	// The destination takes part in its own merge, so PFMERGE accumulates
	// rather than replacing what is already there.
	if existing, exist := hllStore[dest]; exist {
		merged.Merge(existing)
	}
	for _, key := range args[1:] {
		if hll, exist := hllStore[key]; exist {
			merged.Merge(hll)
		}
	}

	hllStore[dest] = merged
	return constant.RespOk
}
