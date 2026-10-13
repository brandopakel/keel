package core

import (
	"errors"
	"math"
	"strconv"
)

// Count-min sketch commands, with the names and replies of RedisBloom's CMS.*
// family: the command language over the typed operations of cms.go. Each
// reads its arguments, calls its operation, and answers through the engine's
// reply sink; a write records itself, and is never logged as it arrived.

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

// cmdCMSINITBYDIM implements CMS.INITBYDIM key width depth.
func (e *Engine) cmdCMSINITBYDIM(args []string) []byte {
	e.recordsItself()
	if len(args) != 3 {
		return e.encode(wrongArguments("CMS.INITBYDIM"), false)
	}
	width, err := strconv.ParseUint(args[1], 10, 32)
	if err != nil {
		return e.encode(errCMSWidth, false)
	}
	depth, err := strconv.ParseUint(args[2], 10, 32)
	if err != nil {
		return e.encode(errCMSDepth, false)
	}
	r := e.reply()
	if err := e.opCMSInitByDim(args[0], uint32(width), uint32(depth), args[1:]); err != nil {
		return r.Error(err).Bytes()
	}
	return r.Status("OK").Bytes()
}

// cmdCMSINITBYPROB implements CMS.INITBYPROB key error probability: the
// sketch is sized so estimates are within error of the total count with the
// given probability of failing to be.
func (e *Engine) cmdCMSINITBYPROB(args []string) []byte {
	e.recordsItself()
	if len(args) != 3 {
		return e.encode(wrongArguments("CMS.INITBYPROB"), false)
	}
	// NaN parses and compares false against both bounds, so both rates refuse
	// it by name.
	errRate, err := strconv.ParseFloat(args[1], 64)
	if err != nil || math.IsNaN(errRate) || errRate <= 0 || errRate >= 1 {
		return e.encode(errCMSErrRate, false)
	}
	prob, err := strconv.ParseFloat(args[2], 64)
	if err != nil || math.IsNaN(prob) || prob <= 0 || prob >= 1 {
		return e.encode(errCMSProb, false)
	}
	r := e.reply()
	if err := e.opCMSInitByProb(args[0], errRate, prob, args[1:]); err != nil {
		return r.Error(err).Bytes()
	}
	return r.Status("OK").Bytes()
}

// cmdCMSINCRBY implements CMS.INCRBY key item increment [item increment ...]:
// one estimate per item after its increment. Every increment is parsed before
// any is applied, so a bad one changes nothing, and a counter that saturates
// reports an error in its position rather than a number that is wrong.
func (e *Engine) cmdCMSINCRBY(args []string) []byte {
	e.recordsItself()
	if len(args) < 3 || len(args)%2 == 0 {
		return e.encode(wrongArguments("CMS.INCRBY"), false)
	}
	key, pairs := args[0], args[1:]
	r := e.reply()
	// RedisBloom opens the key before it reads an increment, so a missing
	// sketch is refused ahead of a malformed increment.
	if _, ok := e.cmsFor(key); !ok {
		return r.Error(errCMSMissing).Bytes()
	}
	items := make([]string, 0, len(pairs)/2)
	increments := make([]uint32, 0, len(pairs)/2)
	for i := 1; i < len(pairs); i += 2 {
		n, err := strconv.ParseUint(pairs[i], 10, 32)
		if err != nil {
			return r.Error(errCMSNumber).Bytes()
		}
		items = append(items, pairs[i-1])
		increments = append(increments, uint32(n))
	}
	r.Array(len(items))
	_ = e.opCMSIncrBy(key, items, increments, pairs, func(estimate uint32, saturated bool) {
		if saturated {
			r.Error(errCMSOverflow)
			return
		}
		r.Int(int64(estimate))
	})
	return r.Bytes()
}

// cmdCMSQUERY implements CMS.QUERY key item [item ...]: one estimate per item.
func (e *Engine) cmdCMSQUERY(args []string) []byte {
	if len(args) < 2 {
		return e.encode(wrongArguments("CMS.QUERY"), false)
	}
	r := e.reply()
	items := args[1:]
	started := false
	err := e.opCMSQuery(args[0], items, func(count uint32) {
		if !started {
			r.Array(len(items))
			started = true
		}
		r.Int(int64(count))
	})
	if err != nil {
		return r.Error(err).Bytes()
	}
	return r.Bytes()
}
