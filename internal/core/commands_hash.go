package core

import (
	"math/big"
	"strings"

	"github.com/brandopakel/keel/internal/constant"
	"github.com/brandopakel/keel/internal/resp"
)

// Hash commands: the command language over the typed operations of
// hashes.go. Each reads its arguments as Redis reads them, calls its
// operation, and answers through the reply sink; HKEYS, HVALS and HGETALL
// keep the walk that answers a whole hash (hashReply).

func (e *Engine) cmdHSET(args []string) []byte {
	e.recordsItself()
	if len(args) < 3 || len(args)%2 != 1 {
		return e.encode(wrongArguments("HSET"), false)
	}
	r := e.reply()
	return r.Int(e.opHSet(args[0], args[1:])).Bytes()
}

func (e *Engine) cmdHSETNX(args []string) []byte {
	e.recordsItself()
	if len(args) != 3 {
		return e.encode(wrongArguments("HSETNX"), false)
	}
	r := e.reply()
	if e.opHSetNX(args[0], args[1], args[2]) {
		return r.Int(1).Bytes()
	}
	return r.Int(0).Bytes()
}

// cmdHGET answers one value, so without a Reply, as GET does
// (resp.SingleBulk).
func (e *Engine) cmdHGET(args []string) []byte {
	if len(args) != 2 {
		return e.encode(wrongArguments("HGET"), false)
	}
	if value, ok := e.opHGet(args[0], args[1]); ok {
		return resp.SingleBulk(MaxReplyBytes-e.replyCeiling, e.commandAllocations, value)
	}
	return e.wire().Null()
}

func (e *Engine) cmdHMGET(args []string) []byte {
	if len(args) < 2 {
		return e.encode(wrongArguments("HMGET"), false)
	}
	h, ok := e.hashFor(args[0])
	return e.encodeLookupArray(len(args)-1, func(i int) (string, bool) {
		if !ok {
			return "", false
		}
		return h.Get(args[i+1])
	}, shapeArray)
}

func (e *Engine) cmdHDEL(args []string) []byte {
	e.recordsItself()
	if len(args) < 2 {
		return e.encode(wrongArguments("HDEL"), false)
	}
	r := e.reply()
	return r.Int(e.opHDel(args[0], args[1:])).Bytes()
}

func (e *Engine) cmdHEXISTS(args []string) []byte {
	if len(args) != 2 {
		return e.encode(wrongArguments("HEXISTS"), false)
	}
	r := e.reply()
	if e.opHExists(args[0], args[1]) {
		return r.Int(1).Bytes()
	}
	return r.Int(0).Bytes()
}

func (e *Engine) cmdHLEN(args []string) []byte {
	if len(args) != 1 {
		return e.encode(wrongArguments("HLEN"), false)
	}
	r := e.reply()
	return r.Int(e.opHLen(args[0])).Bytes()
}

func (e *Engine) cmdHKEYS(args []string) []byte {
	if len(args) != 1 {
		return e.encode(wrongArguments("HKEYS"), false)
	}
	h, ok := e.hashFor(args[0])
	if !ok {
		r := e.reply()
		return r.Raw(constant.RespEmptyArray).Bytes()
	}
	return e.hashReply(h, true, false)
}

func (e *Engine) cmdHVALS(args []string) []byte {
	if len(args) != 1 {
		return e.encode(wrongArguments("HVALS"), false)
	}
	h, ok := e.hashFor(args[0])
	if !ok {
		r := e.reply()
		return r.Raw(constant.RespEmptyArray).Bytes()
	}
	return e.hashReply(h, false, true)
}

// cmdHGETALL answers a map of field to value: in RESP2 a flat array of field,
// value, field, value.
//
// Flat rather than nested because that is what RESP2 clients decode into a map,
// and it is what Redis sends.
func (e *Engine) cmdHGETALL(args []string) []byte {
	if len(args) != 1 {
		return e.encode(wrongArguments("HGETALL"), false)
	}
	h, ok := e.hashFor(args[0])
	if !ok {
		r := e.reply()
		return r.EmptyMap().Bytes()
	}
	return e.hashReply(h, true, true)
}

// cmdHINCRBY adds to a field, treating a missing key or field as zero; see
// opHIncrBy. The increment is read before the key is looked at, as Redis
// reads it.
func (e *Engine) cmdHINCRBY(args []string) []byte {
	e.recordsItself()
	if len(args) != 3 {
		return e.encode(wrongArguments("HINCRBY"), false)
	}
	delta, valid := e.counterInteger(args[2])
	if !valid {
		return e.encode(errNotAnInteger, false)
	}
	return e.counterReply(e.opHIncrBy(args[0], args[1], delta))
}

// cmdHMSET is HSET answering OK, the older spelling; it is recorded as the
// HSET it performs.
func (e *Engine) cmdHMSET(args []string) []byte {
	e.recordsItself()
	if len(args) < 3 || len(args)%2 != 1 {
		return e.encode(wrongArguments("HMSET"), false)
	}
	e.opHSet(args[0], args[1:])
	r := e.reply()
	return r.Status("OK").Bytes()
}

func (e *Engine) cmdHSTRLEN(args []string) []byte {
	if len(args) != 2 {
		return e.encode(wrongArguments("HSTRLEN"), false)
	}
	r := e.reply()
	return r.Int(e.opHStrlen(args[0], args[1])).Bytes()
}

// hincrByFloatIncrement reads HINCRBYFLOAT's increment as Redis's
// hincrbyfloatCommand does, before it looks at the key: a long double, and
// neither NaN nor infinite.
func hincrByFloatIncrement(text string) (*big.Float, error) {
	incr, ok := parseLongDouble(text)
	if !ok {
		return nil, errNotAFloat
	}
	if incr.IsInf() {
		return nil, errNaNOrInfinity
	}
	return incr, nil
}

// cmdHINCRBYFLOAT adds to a field in Redis's long double; see
// opHIncrByFloat. The sum's reply is admitted before it is stored.
func (e *Engine) cmdHINCRBYFLOAT(args []string) []byte {
	e.recordsItself()
	if len(args) != 3 {
		return e.encode(wrongArguments("HINCRBYFLOAT"), false)
	}
	incr, err := hincrByFloatIncrement(args[2])
	if err != nil {
		return e.encode(err, false)
	}
	r := e.reply()
	value, err := e.hashFloatSum(args[0], args[1], incr)
	if err != nil {
		return r.Error(err).Bytes()
	}
	if r.Bulk(value).Refused() {
		return r.Bytes()
	}
	e.storeHashField(args[0], args[1], value)
	return r.Bytes()
}

// cmdHGETDEL implements HGETDEL key FIELDS numfields field [field ...]: the
// values of the fields, null for one not there, and the fields deleted. The
// reply is built and admitted before anything is deleted.
func (e *Engine) cmdHGETDEL(args []string) []byte {
	e.recordsItself()
	if len(args) < 4 {
		return e.encode(wrongArguments("HGETDEL"), false)
	}
	if !strings.EqualFold(args[1], "FIELDS") {
		return e.encode(errHGetDelFields, false)
	}
	n, valid := e.counterInteger(args[2])
	if !valid || n < 1 {
		return e.encode(errHGetDelCount, false)
	}
	fields := args[3:]
	if n != int64(len(fields)) {
		return e.encode(errHGetDelCountsMatch, false)
	}
	values, found := e.hashFieldsGotten(args[0], fields)
	r := e.reply()
	size := resp.DecimalDigits(len(fields)) + 3
	for i := range fields {
		if found[i] {
			size, _ = resp.AddBulkSize(size, len(values[i]))
		} else {
			size += r.Framing.NullSize()
		}
	}
	if !r.Reserve(size) {
		return r.Bytes()
	}
	r.Array(len(fields))
	for i := range fields {
		if found[i] {
			r.Bulk(values[i])
		} else {
			r.Null()
		}
	}
	e.deleteGottenFields(args[0], fields, found)
	return r.Bytes()
}
