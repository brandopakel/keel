package core

import (
	"errors"
	"math"
	"strconv"
	"strings"

	"github.com/brandopakel/keel/internal/constant"
	"github.com/brandopakel/keel/internal/data_structure"
)

// Bloom filter commands, with the names, replies and refusals of RedisBloom
// 8.10.1's BF.* family, so a client written for that module works here: the
// same reply types in RESP2 and RESP3, the same error text, and the same
// answer at each edge - see redisbloom.go.

var (
	errBFBadErrorRate    = errors.New("ERR bad error rate")
	errBFErrorRateRange  = errors.New("ERR error rate must be in the range (0.000000, 1.000000)")
	errBFBadCapacity     = errors.New("ERR bad capacity")
	errBFCapacityRange   = errors.New("ERR capacity must be in the range [1, 1073741824]")
	errBFNoExpansion     = errors.New("ERR no expansion")
	errBFBadExpansion    = errors.New("ERR bad expansion")
	errBFNonScalingGrow  = errors.New("Nonscaling filters cannot expand")
	errBFExpansionRange  = errors.New("ERR expansion must be in the range [0, 32768]")
	errBFCouldNotCreate  = errors.New("ERR could not create filter")
	errBFInvalidInfoName = errors.New("Invalid information value")
)

// RedisBloom's limits on a reservation: bf-initial-size and
// bf-expansion-factor's ranges.
const (
	bfMaxCapacity  = 1 << 30
	bfMaxExpansion = 32768
)

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

// bfReservation is what BF.RESERVE asks for. An expansion of zero is a filter
// that does not grow.
type bfReservation struct {
	errorRate float64
	capacity  uint64
	expansion uint64
}

// parseBFReserve reads BF.RESERVE key error_rate capacity [EXPANSION n]
// [NONSCALING] as RedisBloom does, refusing what it refuses in the order it
// checks: the error rate, the capacity, then the options, which it finds
// anywhere among the arguments and of which it ignores any it does not know.
// An expansion of 0 is NONSCALING.
//
// RedisBloom also lowers an error rate above 0.25 to 0.25. That changes how a
// filter is built, not any reply, and the filters a log or a dump already
// holds were built without it, so a rate here is the rate the filter gets.
func parseBFReserve(args []string) (bfReservation, error) {
	rate, ok := redisDouble(args[1])
	if !ok {
		return bfReservation{}, errBFBadErrorRate
	}
	if !(rate > 0 && rate < 1) {
		return bfReservation{}, errBFErrorRateRange
	}
	capacity, ok := redisInteger(args[2])
	if !ok {
		return bfReservation{}, errBFBadCapacity
	}
	if capacity < 1 || capacity > bfMaxCapacity {
		return bfReservation{}, errBFCapacityRange
	}
	nonScaling := moduleArgIndex("NONSCALING", args) >= 0
	expansion := int64(data_structure.BfDefaultExpansion)
	if at := moduleArgIndex("EXPANSION", args); at >= 0 {
		if at == len(args)-1 {
			return bfReservation{}, errBFNoExpansion
		}
		if expansion, ok = redisInteger(args[at+1]); !ok {
			return bfReservation{}, errBFBadExpansion
		}
		if expansion == 0 {
			nonScaling = true
		} else if nonScaling {
			return bfReservation{}, errBFNonScalingGrow
		}
		if expansion < 0 || expansion > bfMaxExpansion {
			return bfReservation{}, errBFExpansionRange
		}
	}
	if nonScaling {
		expansion = 0
	}
	return bfReservation{errorRate: rate, capacity: uint64(capacity), expansion: uint64(expansion)}, nil
}

// parseLegacyBFReserve is BF.RESERVE as the build before RedisBloom parity
// read it: strconv's numbers, an expansion of 1 to 2^32-1, and nothing but
// EXPANSION after the capacity. It reads only what a log can hold - see
// replayingFilterLog.
func parseLegacyBFReserve(args []string) (bfReservation, bool) {
	if len(args) != 3 && len(args) != 5 {
		return bfReservation{}, false
	}
	rate, err := strconv.ParseFloat(args[1], 64)
	if err != nil || math.IsNaN(rate) || rate <= 0 || rate >= 1 {
		return bfReservation{}, false
	}
	capacity, err := strconv.ParseUint(args[2], 10, 64)
	if err != nil || capacity == 0 {
		return bfReservation{}, false
	}
	expansion := uint64(data_structure.BfDefaultExpansion)
	if len(args) == 5 {
		if !strings.EqualFold(args[3], "EXPANSION") {
			return bfReservation{}, false
		}
		if expansion, err = strconv.ParseUint(args[4], 10, 32); err != nil || expansion < 1 {
			return bfReservation{}, false
		}
	}
	return bfReservation{errorRate: rate, capacity: capacity, expansion: expansion}, true
}

// cmdBFRESERVE implements BF.RESERVE key error_rate capacity [EXPANSION n]
// [NONSCALING].
func (e *Engine) cmdBFRESERVE(args []string) []byte {
	if len(args) < 3 || len(args) > 6 {
		return e.encode(wrongArguments("BF.RESERVE"), false)
	}
	key := args[0]
	// A form the earlier build accepted, replayed, means what it meant to
	// that build - see replayingFilterLog. Where this build reads one of those
	// forms differently, it logs another, below.
	legacy, legacyForm := parseLegacyBFReserve(args)
	r := legacy
	if !legacyForm || !e.replayingFilterLog() {
		var err error
		if r, err = parseBFReserve(args); err != nil {
			return e.encode(err, false)
		}
	}
	switch e.filterKeyStatus(key, e.sbStore) {
	case filterHeld:
		return e.encode(errFilterExists, false)
	case filterOtherType:
		return e.encode(errWrongType, false)
	}
	// RedisBloom sizes the first filter of a chain that grows for half the
	// rate asked for, and cannot build one for a rate that halves to zero.
	// The earlier build could, so a log may hold one.
	if r.expansion != 0 && r.errorRate/2 == 0 && !e.replayingFilterLog() {
		return e.encode(errBFCouldNotCreate, false)
	}
	if err := e.affordable(data_structure.BloomBytesFor(r.capacity, r.errorRate)); err != nil {
		return e.encode(err, false)
	}
	e.sbStore.Put(key, data_structure.CreateSBChain(r.capacity, r.errorRate, r.expansion))
	if legacyForm && legacy != r {
		// The one form both builds read, differently: a key named NONSCALING,
		// which RedisBloom takes for the option and the earlier build took for
		// a name. The log gets the option spelled out, a form the earlier
		// build refused, so a replay builds what this command built.
		e.aofRecord("BF.RESERVE", key, args[1], args[2], "NONSCALING")
	}
	return constant.RespOk
}

// cmdBFADD implements BF.ADD key item: 1 (true) if the item was new, 0 (false)
// if it may have been there already.
func (e *Engine) cmdBFADD(args []string) []byte {
	if len(args) != 2 {
		return e.encode(wrongArguments("BF.ADD"), false)
	}
	key := args[0]
	if e.filterKeyStatus(key, e.sbStore) == filterOtherType {
		return e.encode(errWrongType, false)
	}
	sb := e.bloomForWrite(key)
	added, err := sb.Add(args[1])
	// A scalable Bloom filter grows by adding filters as it fills, so its size
	// after this command is not the size the store recorded on Put.
	e.sbStore.Resize(key)
	if err != nil {
		return e.encode(err, false)
	}
	return e.boolReply(added)
}

// cmdBFMADD implements BF.MADD key item [item ...]: one answer per item, as
// BF.ADD would have answered for it.
func (e *Engine) cmdBFMADD(args []string) []byte {
	if len(args) < 2 {
		return e.encode(wrongArguments("BF.MADD"), false)
	}
	key := args[0]
	if e.filterKeyStatus(key, e.sbStore) == filterOtherType {
		return e.encode(errWrongType, false)
	}
	sb := e.bloomForWrite(key)
	out := make([]interface{}, 0, len(args)-1)
	for _, item := range args[1:] {
		added, err := sb.Add(item)
		if err == data_structure.ErrNonScalingFull {
			// A filter that does not grow is full. RedisBloom answers the
			// items before this one and stops, so the reply ends with the
			// refusal and the items after it are not tried.
			out = append(out, err)
			break
		}
		if err != nil {
			// The filter could not grow to take this item. The error stands in
			// its position, as RedisBloom answers a filter that cannot take
			// more, and the items after it are tried in turn: one that is
			// already present still answers 0.
			out = append(out, err)
			continue
		}
		out = append(out, ReplyBool(added))
	}
	e.sbStore.Resize(key)
	return e.encode(out, false)
}

// bloomForRead is the filter a read asks about: RedisBloom's BF.EXISTS and
// BF.MEXISTS answer no for a key that holds anything else.
func (e *Engine) bloomForRead(key string) (*data_structure.SBChain, bool) {
	if e.filterKeyStatus(key, e.sbStore) != filterHeld {
		return nil, false
	}
	return e.bloomFor(key)
}

func (e *Engine) cmdBFEXISTS(args []string) []byte {
	if len(args) != 2 {
		return e.encode(wrongArguments("BF.EXISTS"), false)
	}
	sb, ok := e.bloomForRead(args[0])
	return e.boolReply(ok && sb.Exists(args[1]))
}

func (e *Engine) cmdBFMEXISTS(args []string) []byte {
	if len(args) < 2 {
		return e.encode(wrongArguments("BF.MEXISTS"), false)
	}
	sb, ok := e.bloomForRead(args[0])
	out := make([]interface{}, 0, len(args)-1)
	for _, item := range args[1:] {
		out = append(out, ReplyBool(ok && sb.Exists(item)))
	}
	return e.encode(out, false)
}

// bfInfoOptions are BF.INFO's single-field names, in the order of the fields
// of the whole reply they pick out.
var bfInfoOptions = []string{"capacity", "size", "filters", "items", "expansion"}

// cmdBFINFO implements BF.INFO key [CAPACITY | SIZE | FILTERS | ITEMS |
// EXPANSION]: name and value pairs describing the filter, or the one asked for.
// A filter that does not grow has no expansion rate, and answers null for it.
func (e *Engine) cmdBFINFO(args []string) []byte {
	if len(args) != 1 && len(args) != 2 {
		return e.encode(wrongArguments("BF.INFO"), false)
	}
	switch e.filterKeyStatus(args[0], e.sbStore) {
	case filterMissing:
		return e.encode(errFilterNotFound, false)
	case filterOtherType:
		return e.encode(errWrongType, false)
	}
	// Peek rather than Get: reporting on a key is not using it.
	sb, _ := e.sbStore.Peek(args[0])
	var expansion interface{}
	if !sb.NonScaling() {
		expansion = int64(sb.Expansion())
	}
	entries := []infoEntry{
		{"Capacity", int64(sb.Capacity())},
		{"Size", int64(sb.MemUsage())},
		{"Number of filters", int64(sb.Filters())},
		{"Number of items inserted", int64(sb.Count())},
		{"Expansion rate", expansion},
	}
	if len(args) == 1 {
		return e.infoReply(entries)
	}
	for i, option := range bfInfoOptions {
		if strings.EqualFold(args[1], option) {
			return e.infoFieldReply(entries[i])
		}
	}
	return e.encode(errBFInvalidInfoName, false)
}
