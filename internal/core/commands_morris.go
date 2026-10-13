package core

import (
	"fmt"
	"math"
	"strconv"

	"github.com/brandopakel/keel/internal/data_structure"
)

// Morris counter commands, the command language over the typed operations of
// morris.go, in the shape the Count-Min sketch commands next door use, because the two answer the same question and the point is to be able to
// swap one for the other and compare.
//
// CMS.INCRBY and MORRIS.INCRBY count a stream of named items in a table of
// fixed size. The difference is the cell: four exact bytes against one
// approximate byte. For the same width and depth a Morris table costs a quarter
// as much and reaches almost the same maximum count, and pays for it with a 20%
// relative error on every read. Which is the better trade depends entirely on
// what the count is for - ranking heavy hitters barely notices, billing does.

func (e *Engine) cmdMORRISINITBYDIM(args []string) []byte {
	e.recordsItself()
	if len(args) != 3 {
		return e.encode(wrongArguments("MORRIS.INITBYDIM"), false)
	}
	width, err := strconv.ParseUint(args[1], 10, 32)
	if err != nil || width == 0 {
		return e.encode(fmt.Errorf("width must be a positive integer number %s", args[1]), false)
	}
	depth, err := strconv.ParseUint(args[2], 10, 32)
	if err != nil || depth == 0 {
		return e.encode(fmt.Errorf("depth must be a positive integer number %s", args[2]), false)
	}
	r := e.reply()
	if err := e.opMorrisInitByDim(args[0], uint32(width), uint32(depth), args[1:]); err != nil {
		return r.Error(err).Bytes()
	}
	return r.Status("OK").Bytes()
}

func (e *Engine) cmdMORRISINITBYPROB(args []string) []byte {
	e.recordsItself()
	if len(args) != 3 {
		return e.encode(wrongArguments("MORRIS.INITBYPROB"), false)
	}
	errRate, err := strconv.ParseFloat(args[1], 64)
	if err != nil {
		return e.encode(fmt.Errorf("errRate must be a floating point number %s", args[1]), false)
	}
	if math.IsNaN(errRate) || errRate >= 1 || errRate <= 0 {
		return e.encode(errMorrisErrRate, false)
	}
	probability, err := strconv.ParseFloat(args[2], 64)
	if err != nil {
		return e.encode(fmt.Errorf("probability must be a floating point number %s", args[2]), false)
	}
	if math.IsNaN(probability) || probability >= 1 || probability <= 0 {
		return e.encode(errMorrisProb, false)
	}
	r := e.reply()
	if err := e.opMorrisInitByProb(args[0], errRate, probability, args[1:]); err != nil {
		return r.Error(err).Bytes()
	}
	return r.Status("OK").Bytes()
}

// cmdMORRISINCRBY answers each item's estimate after its increment, as a
// bulk string, the shape MORRIS.QUERY answers in. Every increment is read
// before the table is touched, and the table is looked up first, so a missing
// one is refused ahead of a malformed increment.
func (e *Engine) cmdMORRISINCRBY(args []string) []byte {
	e.recordsItself()
	if len(args) < 3 || len(args)%2 == 0 {
		return e.encode(wrongArguments("MORRIS.INCRBY"), false)
	}
	key, pairs := args[0], args[1:]
	r := e.reply()
	if !e.morrisStore.Exists(key) {
		return r.Error(errMorrisMissing).Bytes()
	}
	items := make([]string, 0, len(pairs)/2)
	increments := make([]uint64, 0, len(pairs)/2)
	for i := 1; i < len(pairs); i += 2 {
		value, err := strconv.ParseUint(pairs[i], 10, 64)
		if err != nil {
			return r.Error(errMorrisIncrement).Bytes()
		}
		items = append(items, pairs[i-1])
		increments = append(increments, value)
	}
	r.Array(len(items))
	_ = e.opMorrisIncrBy(key, items, increments, pairs, func(estimate uint64) {
		r.Bulk(strconv.FormatUint(estimate, 10))
	})
	return r.Bytes()
}

func (e *Engine) cmdMORRISQUERY(args []string) []byte {
	if len(args) < 2 {
		return e.encode(wrongArguments("MORRIS.QUERY"), false)
	}
	items := args[1:]
	r := e.reply()
	started := false
	err := e.opMorrisQuery(args[0], items, func(count uint64) {
		if !started {
			r.Array(len(items))
			started = true
		}
		r.Bulk(strconv.FormatUint(count, 10))
	})
	if err != nil {
		return r.Error(err).Bytes()
	}
	return r.Bytes()
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
	// A keyspace hit or a miss, as CMS.INFO's is in RedisBloom.
	_, held := e.space.OwnerOf(key)
	e.noteLookup(held)
	m, exist := e.morrisStore.Peek(key)
	if !exist {
		return e.encode(fmt.Errorf("Morris counter with key '%s' does not exist", key), false)
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
