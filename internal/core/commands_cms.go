package core

import (
	"errors"
	"math"
	"strconv"
	"strings"
)

// Count-min sketch commands, with the names, replies and refusals of
// RedisBloom 8.10.1's CMS.* family (rm_cms.c): the command language over the
// typed operations of cms.go. Each reads its arguments as RedisBloom reads
// them, with Redis's string2ll and string2d for its numbers, calls its
// operation, and answers through the engine's reply sink; a write records
// itself, and is never logged as it arrived.
//
// A log written before this followed RedisBloom read some numbers more
// loosely - a width, a depth or an increment written 007, a rate strconv
// reads and Redis does not - and recorded them. Replayed, a command in such a
// form is read as that build read it (replayingFilterLog).

var (
	errCMSExists     = errors.New("CMS: key already exists")
	errCMSMissing    = errors.New("CMS: key does not exist")
	errCMSWidth      = errors.New("CMS: invalid width")
	errCMSDepth      = errors.New("CMS: invalid depth")
	errCMSInitArgs   = errors.New("CMS: invalid init arguments")
	errCMSErrRate    = errors.New("CMS: invalid overestimation value")
	errCMSProb       = errors.New("CMS: invalid prob value")
	errCMSNumber     = errors.New("CMS: Cannot parse number")
	errCMSNegative   = errors.New("CMS: Number cannot be negative")
	errCMSOverflow   = errors.New("CMS: INCRBY overflow")
	errCMSNumKeys    = errors.New("CMS: invalid numkeys")
	errCMSKeysCount  = errors.New("CMS: Number of keys must be positive")
	errCMSKeysWrong  = errors.New("CMS: wrong number of keys")
	errCMSKeysWeight = errors.New("CMS: wrong number of keys/weights")
	errCMSWeight     = errors.New("CMS: invalid weight value")
	errCMSDimensions = errors.New("CMS: width/depth is not equal")
)

// cmsDimension reads a width or a depth, a whole number of at least 1 as
// RedisBloom reads one, or as the build before read it for a log's replay.
func (e *Engine) cmsDimension(text string, refusal error) (int64, error) {
	if n, ok := redisInteger(text); ok && n >= 1 {
		return n, nil
	}
	if e.replayingFilterLog() {
		if n, err := strconv.ParseUint(text, 10, 32); err == nil && n >= 1 {
			return int64(n), nil
		}
	}
	return 0, refusal
}

// cmsRate reads a rate strictly between 0 and 1, as RedisBloom reads one.
func (e *Engine) cmsRate(text string, refusal error) (float64, error) {
	rate, ok := redisDouble(text)
	if !ok && e.replayingFilterLog() {
		var err error
		rate, err = strconv.ParseFloat(text, 64)
		ok = err == nil && !math.IsNaN(rate)
	}
	if !ok || rate <= 0 || rate >= 1 {
		return 0, refusal
	}
	return rate, nil
}

// cmdCMSINITBYDIM implements CMS.INITBYDIM key width depth. RedisBloom finds
// the key taken, by anything, before it reads the dimensions.
func (e *Engine) cmdCMSINITBYDIM(args []string) []byte {
	e.recordsItself()
	if len(args) != 3 {
		return e.encode(wrongArguments("CMS.INITBYDIM"), false)
	}
	r := e.reply()
	if _, held := e.space.OwnerOf(args[0]); held {
		return r.Error(errCMSExists).Bytes()
	}
	width, err := e.cmsDimension(args[1], errCMSWidth)
	if err != nil {
		return r.Error(err).Bytes()
	}
	depth, err := e.cmsDimension(args[2], errCMSDepth)
	if err != nil {
		return r.Error(err).Bytes()
	}
	if err := e.opCMSInitByDim(args[0], width, depth, args[1:]); err != nil {
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
	r := e.reply()
	if _, held := e.space.OwnerOf(args[0]); held {
		return r.Error(errCMSExists).Bytes()
	}
	errRate, err := e.cmsRate(args[1], errCMSErrRate)
	if err != nil {
		return r.Error(err).Bytes()
	}
	prob, err := e.cmsRate(args[2], errCMSProb)
	if err != nil {
		return r.Error(err).Bytes()
	}
	if err := e.opCMSInitByProb(args[0], errRate, prob, args[1:]); err != nil {
		return r.Error(err).Bytes()
	}
	return r.Status("OK").Bytes()
}

// cmsIncrement reads one of CMS.INCRBY's increments, a whole number of zero
// or more as RedisBloom reads one, or as the build before read it for a log's
// replay.
func (e *Engine) cmsIncrement(text string) (uint64, error) {
	n, ok := redisInteger(text)
	if !ok && e.replayingFilterLog() {
		if v, err := strconv.ParseUint(text, 10, 32); err == nil {
			return v, nil
		}
	}
	switch {
	case !ok:
		return 0, errCMSNumber
	case n < 0:
		return 0, errCMSNegative
	}
	return uint64(n), nil
}

// cmdCMSINCRBY implements CMS.INCRBY key item increment [item increment ...]:
// one estimate per item after its increment, or the overflow error in its
// place where a counter could not take it. The key is looked at first, and
// every increment read before any is applied, so a bad one changes nothing.
func (e *Engine) cmdCMSINCRBY(args []string) []byte {
	e.recordsItself()
	if len(args) < 3 || len(args)%2 == 0 {
		return e.encode(wrongArguments("CMS.INCRBY"), false)
	}
	key, pairs := args[0], args[1:]
	r := e.reply()
	if _, ok := e.cmsFor(key); !ok {
		return r.Error(errCMSMissing).Bytes()
	}
	items := make([]string, 0, len(pairs)/2)
	increments := make([]uint64, 0, len(pairs)/2)
	for i := 1; i < len(pairs); i += 2 {
		n, err := e.cmsIncrement(pairs[i])
		if err != nil {
			return r.Error(err).Bytes()
		}
		items = append(items, pairs[i-1])
		increments = append(increments, n)
	}
	r.Array(len(items))
	_ = e.opCMSIncrBy(key, items, increments, pairs, func(estimate uint32) {
		if estimate == math.MaxUint32 {
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

// cmdCMSINFO implements CMS.INFO key: the sketch's width, depth and total
// count, a map of simple-string names in RESP3 and their flat array in RESP2,
// as RedisBloom's CMSKetch_Info answers.
func (e *Engine) cmdCMSINFO(args []string) []byte {
	if len(args) != 1 {
		return e.encode(wrongArguments("CMS.INFO"), false)
	}
	info, err := e.opCMSInfo(args[0])
	if err != nil {
		return e.encode(err, false)
	}
	return e.infoReply([]infoEntry{
		{"width", int64(info.width)},
		{"depth", int64(info.depth)},
		{"count", int64(info.count)},
	})
}

// cmdCMSMERGE implements CMS.MERGE destination numKeys source [source ...]
// [WEIGHTS weight [weight ...]], reading it in RedisBloom's order: the
// destination, numKeys, the shape of the rest - WEIGHTS found anywhere after
// numKeys, as RMUtil_ArgIndex finds it - and then each source with its
// weight, in turn. See opCMSMerge.
func (e *Engine) cmdCMSMERGE(args []string) []byte {
	e.recordsItself()
	if len(args) < 3 {
		return e.encode(wrongArguments("CMS.MERGE"), false)
	}
	dest, rest := args[0], args[2:]
	r := e.reply()
	if _, ok := e.cmsFor(dest); !ok {
		return r.Error(errCMSMissing).Bytes()
	}
	numKeys, ok := redisInteger(args[1])
	if !ok {
		return r.Error(errCMSNumKeys).Bytes()
	}
	if numKeys <= 0 {
		return r.Error(errCMSKeysCount).Bytes()
	}
	weights := -1
	for i, arg := range rest {
		if strings.EqualFold(arg, "WEIGHTS") {
			weights = i
			break
		}
	}
	switch {
	case weights < 0 && numKeys != int64(len(rest)):
		return r.Error(errCMSKeysWrong).Bytes()
	case weights >= 0 && (int64(weights) != numKeys || int64(len(rest)) != 1+2*numKeys):
		return r.Error(errCMSKeysWeight).Bytes()
	}
	req := cmsMergeRequest{sources: rest[:numKeys], text: args[1:]}
	if weights >= 0 {
		weightText := rest[numKeys+1:]
		req.weight = func(i int) (int64, error) {
			w, ok := redisInteger(weightText[i])
			if !ok {
				return 0, errCMSWeight
			}
			return w, nil
		}
	}
	if err := e.opCMSMerge(dest, req); err != nil {
		return r.Error(err).Bytes()
	}
	return r.Status("OK").Bytes()
}
