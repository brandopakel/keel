package core

import (
	"errors"
	"fmt"
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

func cmdMORRISINITBYDIM(args []string) []byte {
	if len(args) != 3 {
		return Encode(errors.New("(error) ERR wrong number of arguments for 'MORRIS.INITBYDIM' command"), false)
	}
	key := args[0]
	width, err := strconv.ParseUint(args[1], 10, 32)
	if err != nil || width == 0 {
		return Encode(errors.New(fmt.Sprintf("width must be a positive integer number %s", args[1])), false)
	}
	depth, err := strconv.ParseUint(args[2], 10, 32)
	if err != nil || depth == 0 {
		return Encode(errors.New(fmt.Sprintf("depth must be a positive integer number %s", args[2])), false)
	}
	if morrisStore.Exists(key) {
		return Encode(errors.New("MORRIS: key already exists"), false)
	}
	morrisStore.Put(key, data_structure.CreateMorris(uint32(width), uint32(depth)))
	return constant.RespOk
}

func cmdMORRISINITBYPROB(args []string) []byte {
	if len(args) != 3 {
		return Encode(errors.New("(error) ERR wrong number of arguments for 'MORRIS.INITBYPROB' command"), false)
	}
	key := args[0]
	errRate, err := strconv.ParseFloat(args[1], 64)
	if err != nil {
		return Encode(errors.New(fmt.Sprintf("errRate must be a floating point number %s", args[1])), false)
	}
	if errRate >= 1 || errRate <= 0 {
		return Encode(errors.New("MORRIS: invalid overestimation value"), false)
	}
	probability, err := strconv.ParseFloat(args[2], 64)
	if err != nil {
		return Encode(errors.New(fmt.Sprintf("probability must be a floating point number %s", args[2])), false)
	}
	if probability >= 1 || probability <= 0 {
		return Encode(errors.New("MORRIS: invalid prob value"), false)
	}
	if morrisStore.Exists(key) {
		return Encode(errors.New("MORRIS: key already exists"), false)
	}

	// These bound the error the hashing contributes. The counters contribute
	// their own on top, which no table dimension can reduce - MORRIS.INFO
	// reports it so the two are not confused for each other.
	w, d := data_structure.CalcMorrisDim(errRate, probability)
	morrisStore.Put(key, data_structure.CreateMorris(w, d))
	return constant.RespOk
}

func cmdMORRISINCRBY(args []string) []byte {
	if len(args) < 3 || len(args)%2 == 0 {
		return Encode(errors.New("(error) ERR wrong number of arguments for 'MORRIS.INCRBY' command"), false)
	}
	key := args[0]
	m, exist := morrisStore.Get(key)
	if !exist {
		return Encode(errors.New("MORRIS: key does not exist"), false)
	}

	var res []string
	for i := 1; i < len(args); i += 2 {
		item := args[i]
		value, err := strconv.ParseUint(args[i+1], 10, 64)
		if err != nil {
			return Encode(errors.New(fmt.Sprintf("increment must be a non negative integer number %s", args[i+1])), false)
		}
		res = append(res, fmt.Sprintf("%d", m.IncrBy(item, value)))
	}
	// The table is a fixed size, so unlike a set or a sorted set this cannot
	// change what the key costs - and Resize is deliberately not called, since
	// it would only re-measure a number that has not moved.
	return Encode(res, false)
}

func cmdMORRISQUERY(args []string) []byte {
	if len(args) < 2 {
		return Encode(errors.New("(error) ERR wrong number of arguments for 'MORRIS.QUERY' command"), false)
	}
	key := args[0]
	m, exist := morrisStore.Get(key)
	if !exist {
		return Encode(errors.New("MORRIS: key does not exist"), false)
	}

	var res []string
	for i := 1; i < len(args); i++ {
		res = append(res, fmt.Sprintf("%d", m.Count(args[i])))
	}
	return Encode(res, false)
}

// cmdMORRISINFO reports the shape of the table and, more importantly, the
// accuracy of a single counter.
//
// An estimate read back without that figure invites being read as exact, which
// is the one thing it is not. The comparable Count-Min sketch's memory is
// reported alongside so the trade is legible from the client.
func cmdMORRISINFO(args []string) []byte {
	if len(args) != 1 {
		return Encode(errors.New("(error) ERR wrong number of arguments for 'MORRIS.INFO' command"), false)
	}
	key := args[0]
	m, exist := morrisStore.Peek(key)
	if !exist {
		return Encode(errors.New(fmt.Sprintf("Morris counter with key '%s' does not exist", key)), false)
	}
	res := []string{
		"Width", fmt.Sprintf("%d", m.Width()),
		"Depth", fmt.Sprintf("%d", m.Depth()),
		"Size", fmt.Sprintf("%d", m.MemUsage()),
		"Count-Min equivalent size", fmt.Sprintf("%d", data_structure.CMSMemUsageFor(m.Width(), m.Depth())),
		"Counter relative error", fmt.Sprintf("%.4f", data_structure.MorrisRelativeError),
		"Max count", fmt.Sprintf("%d", data_structure.MorrisMaxCount),
		"Total count", fmt.Sprintf("%d", m.TotalCount()),
	}
	return Encode(res, false)
}
