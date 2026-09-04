package core

import (
	"errors"
	"strconv"
	"strings"

	"github.com/brandopakel/keel/internal/constant"
	"github.com/brandopakel/keel/internal/data_structure"
)

// Bloom filter commands, with the names and replies of RedisBloom's BF.*
// family so a client written for that module works here.

var (
	errBFExists    = errors.New("ERR item exists")
	errBFNotFound  = errors.New("ERR not found")
	errBFErrorRate = errors.New("ERR (0 < error rate range < 1)")
	errBFCapacity  = errors.New("ERR (capacity should be larger than 0)")
	errBFExpansion = errors.New("ERR (expansion should be greater or equal to 1)")
)

func bloomFor(key string) (*data_structure.SBChain, bool) {
	return sbStore.Get(key)
}

// bloomForWrite returns the filter at key, creating one with the default sizing
// if there is none: adding to a filter that was never reserved is how most
// filters come to exist.
func bloomForWrite(key string) *data_structure.SBChain {
	sb, ok := sbStore.Get(key)
	if !ok {
		sb = data_structure.CreateSBChain(data_structure.BfDefaultInitCapacity,
			data_structure.BfDefaultErrRate, data_structure.BfDefaultExpansion)
		sbStore.Put(key, sb)
	}
	return sb
}

// cmdBFRESERVE implements BF.RESERVE key error_rate capacity [EXPANSION n].
func cmdBFRESERVE(args []string) []byte {
	if len(args) != 3 && len(args) != 5 {
		return Encode(errors.New("ERR wrong number of arguments for 'BF.RESERVE' command"), false)
	}
	key := args[0]

	errorRate, err := strconv.ParseFloat(args[1], 64)
	if err != nil {
		return Encode(errors.New("ERR bad error rate"), false)
	}
	if errorRate <= 0 || errorRate >= 1 {
		return Encode(errBFErrorRate, false)
	}
	capacity, err := strconv.ParseUint(args[2], 10, 64)
	if err != nil {
		return Encode(errors.New("ERR bad capacity"), false)
	}
	if capacity == 0 {
		return Encode(errBFCapacity, false)
	}

	expansion := uint64(data_structure.BfDefaultExpansion)
	if len(args) == 5 {
		if !strings.EqualFold(args[3], "EXPANSION") {
			return Encode(errSyntax, false)
		}
		expansion, err = strconv.ParseUint(args[4], 10, 32)
		if err != nil {
			return Encode(errors.New("ERR bad expansion"), false)
		}
		if expansion < 1 {
			return Encode(errBFExpansion, false)
		}
	}

	if sbStore.Exists(key) {
		return Encode(errBFExists, false)
	}
	sbStore.Put(key, data_structure.CreateSBChain(capacity, errorRate, expansion))
	return constant.RespOk
}

// cmdBFADD implements BF.ADD key item: 1 if the item was new, 0 if it may have
// been there already.
func cmdBFADD(args []string) []byte {
	if len(args) != 2 {
		return Encode(errors.New("ERR wrong number of arguments for 'BF.ADD' command"), false)
	}
	key := args[0]
	sb := bloomForWrite(key)
	added := sb.Add(args[1])
	// A scalable Bloom filter grows by adding filters as it fills, so its size
	// after this command is not the size the store recorded on Put.
	sbStore.Resize(key)
	if added {
		return constant.RespOne
	}
	return constant.RespZero
}

// cmdBFMADD implements BF.MADD key item [item ...]: one integer per item, as
// BF.ADD would have answered for it.
func cmdBFMADD(args []string) []byte {
	if len(args) < 2 {
		return Encode(errors.New("ERR wrong number of arguments for 'BF.MADD' command"), false)
	}
	key := args[0]
	sb := bloomForWrite(key)
	out := make([]interface{}, 0, len(args)-1)
	for _, item := range args[1:] {
		if sb.Add(item) {
			out = append(out, int64(1))
		} else {
			out = append(out, int64(0))
		}
	}
	sbStore.Resize(key)
	return Encode(out, false)
}

func cmdBFEXISTS(args []string) []byte {
	if len(args) != 2 {
		return Encode(errors.New("ERR wrong number of arguments for 'BF.EXISTS' command"), false)
	}
	sb, ok := bloomFor(args[0])
	if !ok || !sb.Exists(args[1]) {
		return constant.RespZero
	}
	return constant.RespOne
}

func cmdBFMEXISTS(args []string) []byte {
	if len(args) < 2 {
		return Encode(errors.New("ERR wrong number of arguments for 'BF.MEXISTS' command"), false)
	}
	sb, ok := bloomFor(args[0])
	out := make([]interface{}, 0, len(args)-1)
	for _, item := range args[1:] {
		if ok && sb.Exists(item) {
			out = append(out, int64(1))
		} else {
			out = append(out, int64(0))
		}
	}
	return Encode(out, false)
}

// cmdBFINFO implements BF.INFO key: name and value pairs describing the filter.
func cmdBFINFO(args []string) []byte {
	if len(args) != 1 {
		return Encode(errors.New("ERR wrong number of arguments for 'BF.INFO' command"), false)
	}
	// Peek rather than Get: reporting on a key is not using it.
	sb, ok := sbStore.Peek(args[0])
	if !ok {
		return Encode(errBFNotFound, false)
	}
	return Encode([]interface{}{
		"Capacity", int64(sb.Capacity()),
		"Size", int64(sb.MemUsage()),
		"Number of filters", int64(sb.Filters()),
		"Number of items inserted", int64(sb.Count()),
		"Expansion rate", int64(sb.Expansion()),
	}, false)
}
