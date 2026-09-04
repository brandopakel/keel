package core

import (
	"errors"
	"strconv"

	"github.com/brandopakel/keel/internal/constant"
	"github.com/brandopakel/keel/internal/data_structure"
)

// Count-min sketch commands, with the names and replies of RedisBloom's CMS.*
// family.

var (
	errCMSExists   = errors.New("CMS: key already exists")
	errCMSMissing  = errors.New("CMS: key does not exist")
	errCMSWidth    = errors.New("CMS: invalid width")
	errCMSDepth    = errors.New("CMS: invalid depth")
	errCMSErrRate  = errors.New("CMS: invalid overestimation value")
	errCMSProb     = errors.New("CMS: invalid prob value")
	errCMSNumber   = errors.New("CMS: Cannot parse number")
	errCMSOverflow = errors.New("CMS: INCRBY overflow")
)

func cmsFor(key string) (*data_structure.CMS, bool) {
	return cmsStore.Get(key)
}

// cmsCreate stores a fresh sketch at key, refusing dimensions that are zero or
// that would not fit, and a key already taken. The dimensions arrive from a
// client and multiply into an allocation, so their product is checked against
// what the server will allocate before anything is made.
func cmsCreate(key string, width, depth uint32) []byte {
	if width == 0 {
		return Encode(errCMSWidth, false)
	}
	if depth == 0 {
		return Encode(errCMSDepth, false)
	}
	// The cell count is checked before the byte count: two 32-bit dimensions
	// multiply to a number of cells that fits in 64 bits, but four bytes each
	// can wrap, and a wrapped size would look small.
	if uint64(width)*uint64(depth) > maxStructureBytes/4 {
		return Encode(errTooLargeForOneKey, false)
	}
	if err := affordable(data_structure.CMSMemUsageFor(width, depth)); err != nil {
		return Encode(err, false)
	}
	if cmsStore.Exists(key) {
		return Encode(errCMSExists, false)
	}
	cmsStore.Put(key, data_structure.CreateCMS(width, depth))
	return constant.RespOk
}

// cmdCMSINITBYDIM implements CMS.INITBYDIM key width depth.
func cmdCMSINITBYDIM(args []string) []byte {
	if len(args) != 3 {
		return Encode(errors.New("ERR wrong number of arguments for 'CMS.INITBYDIM' command"), false)
	}
	width, err := strconv.ParseUint(args[1], 10, 32)
	if err != nil {
		return Encode(errCMSWidth, false)
	}
	depth, err := strconv.ParseUint(args[2], 10, 32)
	if err != nil {
		return Encode(errCMSDepth, false)
	}
	return cmsCreate(args[0], uint32(width), uint32(depth))
}

// cmdCMSINITBYPROB implements CMS.INITBYPROB key error probability: the
// sketch is sized so estimates are within error of the total count with the
// given probability of failing to be.
func cmdCMSINITBYPROB(args []string) []byte {
	if len(args) != 3 {
		return Encode(errors.New("ERR wrong number of arguments for 'CMS.INITBYPROB' command"), false)
	}
	errRate, err := strconv.ParseFloat(args[1], 64)
	if err != nil || errRate <= 0 || errRate >= 1 {
		return Encode(errCMSErrRate, false)
	}
	prob, err := strconv.ParseFloat(args[2], 64)
	if err != nil || prob <= 0 || prob >= 1 {
		return Encode(errCMSProb, false)
	}
	width, depth := data_structure.CalcCMSDim(errRate, prob)
	return cmsCreate(args[0], width, depth)
}

// cmdCMSINCRBY implements CMS.INCRBY key item increment [item increment ...]:
// one estimate per item after its increment. Every increment is parsed before
// any is applied, so a bad one changes nothing, and a counter that saturates
// reports an error in its position rather than a number that is wrong.
func cmdCMSINCRBY(args []string) []byte {
	if len(args) < 3 || len(args)%2 == 0 {
		return Encode(errors.New("ERR wrong number of arguments for 'CMS.INCRBY' command"), false)
	}
	cms, ok := cmsFor(args[0])
	if !ok {
		return Encode(errCMSMissing, false)
	}

	pairs := args[1:]
	increments := make([]uint32, 0, len(pairs)/2)
	for i := 1; i < len(pairs); i += 2 {
		n, err := strconv.ParseUint(pairs[i], 10, 32)
		if err != nil {
			return Encode(errCMSNumber, false)
		}
		increments = append(increments, uint32(n))
	}

	out := make([]interface{}, 0, len(increments))
	for i, inc := range increments {
		estimate, saturated := cms.IncrBy(pairs[2*i], inc)
		if saturated {
			out = append(out, errCMSOverflow)
			continue
		}
		out = append(out, int64(estimate))
	}
	return Encode(out, false)
}

// cmdCMSQUERY implements CMS.QUERY key item [item ...]: one estimate per item.
func cmdCMSQUERY(args []string) []byte {
	if len(args) < 2 {
		return Encode(errors.New("ERR wrong number of arguments for 'CMS.QUERY' command"), false)
	}
	cms, ok := cmsFor(args[0])
	if !ok {
		return Encode(errCMSMissing, false)
	}
	out := make([]interface{}, 0, len(args)-1)
	for _, item := range args[1:] {
		out = append(out, int64(cms.Count(item)))
	}
	return Encode(out, false)
}
