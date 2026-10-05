package core

import (
	"errors"
	"fmt"
	"math"
	"strconv"

	"github.com/brandopakel/keel/internal/constant"
	"github.com/brandopakel/keel/internal/data_structure"
)

// Morris counter commands, in the shape the Count-Min sketch commands next door
// use, because the two answer the same question and the point is to be able to
// swap one for the other and compare.
//
// CMS.INCRBY and MORRIS.INCRBY count a stream of named items in a table of
// fixed size. The difference is the cell: four exact bytes against one
// approximate byte. For the same width and depth a Morris table costs a quarter
// as much and reaches almost the same maximum count, and pays for it with a 20%
// relative error on every read. Which is the better trade depends entirely on
// what the count is for - ranking heavy hitters barely notices, billing does.

func (e *Engine) cmdMORRISINITBYDIM(args []string) []byte {
	if len(args) != 3 {
		return e.encode(wrongArguments("MORRIS.INITBYDIM"), false)
	}
	key := args[0]
	width, err := strconv.ParseUint(args[1], 10, 32)
	if err != nil || width == 0 {
		return e.encode(errors.New(fmt.Sprintf("width must be a positive integer number %s", args[1])), false)
	}
	depth, err := strconv.ParseUint(args[2], 10, 32)
	if err != nil || depth == 0 {
		return e.encode(errors.New(fmt.Sprintf("depth must be a positive integer number %s", args[2])), false)
	}
	if e.morrisStore.Exists(key) {
		return e.encode(errors.New("MORRIS: key already exists"), false)
	}
	if err := e.affordable(64 + width*depth); err != nil {
		return e.encode(err, false)
	}
	e.morrisStore.Put(key, data_structure.CreateMorris(uint32(width), uint32(depth)))
	return constant.RespOk
}

func (e *Engine) cmdMORRISINITBYPROB(args []string) []byte {
	if len(args) != 3 {
		return e.encode(wrongArguments("MORRIS.INITBYPROB"), false)
	}
	key := args[0]
	errRate, err := strconv.ParseFloat(args[1], 64)
	if err != nil {
		return e.encode(errors.New(fmt.Sprintf("errRate must be a floating point number %s", args[1])), false)
	}
	if math.IsNaN(errRate) || errRate >= 1 || errRate <= 0 {
		return e.encode(errors.New("MORRIS: invalid overestimation value"), false)
	}
	probability, err := strconv.ParseFloat(args[2], 64)
	if err != nil {
		return e.encode(errors.New(fmt.Sprintf("probability must be a floating point number %s", args[2])), false)
	}
	if math.IsNaN(probability) || probability >= 1 || probability <= 0 {
		return e.encode(errors.New("MORRIS: invalid prob value"), false)
	}
	if e.morrisStore.Exists(key) {
		return e.encode(errors.New("MORRIS: key already exists"), false)
	}

	// These bound the error the hashing contributes. The counters contribute
	// their own on top, which no table dimension can reduce - MORRIS.INFO
	// reports it so the two are not confused for each other.
	w, d := data_structure.CalcMorrisDim(errRate, probability)
	if w == 0 || d == 0 {
		return e.encode(errTooLargeForOneKey, false)
	}
	if err := e.affordable(64 + uint64(w)*uint64(d)); err != nil {
		return e.encode(err, false)
	}
	e.morrisStore.Put(key, data_structure.CreateMorris(w, d))
	return constant.RespOk
}

func (e *Engine) cmdMORRISINCRBY(args []string) []byte {
	if len(args) < 3 || len(args)%2 == 0 {
		return e.encode(wrongArguments("MORRIS.INCRBY"), false)
	}
	key := args[0]
	m, exist := e.morrisStore.Get(key)
	if !exist {
		return e.encode(errors.New("MORRIS: key does not exist"), false)
	}

	increments := make([]uint64, 0, len(args)/2)
	for i := 2; i < len(args); i += 2 {
		value, err := strconv.ParseUint(args[i], 10, 64)
		if err != nil {
			return e.encode(errors.New("ERR increment must be a non negative integer"), false)
		}
		increments = append(increments, value)
	}
	var res []string
	for i, value := range increments {
		res = append(res, strconv.FormatUint(m.IncrBy(args[1+2*i], value), 10))
	}
	// The table is a fixed size, so unlike a set or a sorted set this cannot
	// change what the key costs - and Resize is deliberately not called, since
	// it would only re-measure a number that has not moved.
	return e.encode(res, false)
}

func (e *Engine) cmdMORRISQUERY(args []string) []byte {
	if len(args) < 2 {
		return e.encode(wrongArguments("MORRIS.QUERY"), false)
	}
	key := args[0]
	m, exist := e.morrisStore.Get(key)
	if !exist {
		return e.encode(errors.New("MORRIS: key does not exist"), false)
	}

	var res []string
	for i := 1; i < len(args); i++ {
		res = append(res, fmt.Sprintf("%d", m.Count(args[i])))
	}
	return e.encode(res, false)
}

// cmdMORRISINFO reports the shape of the table and, more importantly, the
// accuracy of a single counter.
//
// An estimate read back without that figure invites being read as exact, which
// is the one thing it is not. The comparable Count-Min sketch's memory is
// reported alongside so the trade is legible from the client.
//
// RedisBloom has no Morris counter to follow under RESP3, so the reply takes
// RESP3's framing for name/value pairs, a map, and keeps the bulk-string names
// and values RESP2 sends - as MORRIS.QUERY keeps its bulk-string counts.
func (e *Engine) cmdMORRISINFO(args []string) []byte {
	if len(args) != 1 {
		return e.encode(wrongArguments("MORRIS.INFO"), false)
	}
	key := args[0]
	m, exist := e.morrisStore.Peek(key)
	if !exist {
		return e.encode(errors.New(fmt.Sprintf("Morris counter with key '%s' does not exist", key)), false)
	}
	res := ReplyMap{
		"Width", fmt.Sprintf("%d", m.Width()),
		"Depth", fmt.Sprintf("%d", m.Depth()),
		"Size", fmt.Sprintf("%d", m.MemUsage()),
		"Count-Min equivalent size", fmt.Sprintf("%d", data_structure.CMSMemUsageFor(m.Width(), m.Depth())),
		"Counter relative error", fmt.Sprintf("%.4f", data_structure.MorrisRelativeError),
		"Max count", fmt.Sprintf("%d", data_structure.MorrisMaxCount),
		"Total count", fmt.Sprintf("%d", m.TotalCount()),
	}
	return e.encode(res, false)
}
