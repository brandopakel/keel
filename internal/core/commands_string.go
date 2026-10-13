package core

import "strings"

// The command language of the string commands Keel did not have: APPEND,
// GETRANGE and SUBSTR, SETRANGE, STRLEN, GETDEL, GETEX, GETSET, MSETNX and
// INCRBYFLOAT. Each reads its arguments in the order Redis 8.10.1's
// t_string.c reads them, calls its typed operation (strings.go), and answers
// through the reply sink. A key of another type is refused before the
// handler runs (keytype.go), after the arguments Redis reads first
// (argumentsBeforeType).

func (e *Engine) cmdAPPEND(args []string) []byte {
	e.recordsItself()
	if len(args) != 2 {
		return e.encode(wrongArguments("APPEND"), false)
	}
	r := e.reply()
	n, err := e.opAppend(args[0], args[1])
	if err != nil {
		return r.Error(err).Bytes()
	}
	return r.Int(n).Bytes()
}

func (e *Engine) cmdSTRLEN(args []string) []byte {
	if len(args) != 1 {
		return e.encode(wrongArguments("STRLEN"), false)
	}
	r := e.reply()
	return r.Int(e.opStrlen(args[0])).Bytes()
}

// cmdGETRANGE implements GETRANGE key start end, and SUBSTR, its older name.
// Both indexes are read before the key is looked at; a missing key answers
// an empty string, not a null.
func (e *Engine) cmdGETRANGE(args []string) []byte { return e.getRange("GETRANGE", args) }
func (e *Engine) cmdSUBSTR(args []string) []byte   { return e.getRange("SUBSTR", args) }

func (e *Engine) getRange(name string, args []string) []byte {
	if len(args) != 3 {
		return e.encode(wrongArguments(name), false)
	}
	start, end, err := e.rangeIndexes(args)
	if err != nil {
		return e.encode(err, false)
	}
	r := e.reply()
	return r.Bulk(e.opGetRange(args[0], start, end)).Bytes()
}

// rangeIndexes reads GETRANGE's start and end as Redis's
// getLongLongFromObjectOrReply reads them.
func (e *Engine) rangeIndexes(args []string) (start, end int64, err error) {
	start, validStart := e.counterInteger(args[1])
	if !validStart {
		return 0, 0, errNotAnInteger
	}
	end, validEnd := e.counterInteger(args[2])
	if !validEnd {
		return 0, 0, errNotAnInteger
	}
	return start, end, nil
}

// cmdSETRANGE implements SETRANGE key offset value. The offset is read, and a
// negative one refused, before the key is looked at.
func (e *Engine) cmdSETRANGE(args []string) []byte {
	e.recordsItself()
	if len(args) != 3 {
		return e.encode(wrongArguments("SETRANGE"), false)
	}
	offset, err := e.setRangeOffset(args)
	if err != nil {
		return e.encode(err, false)
	}
	r := e.reply()
	n, err := e.opSetRange(args[0], offset, args[2])
	if err != nil {
		return r.Error(err).Bytes()
	}
	return r.Int(n).Bytes()
}

func (e *Engine) setRangeOffset(args []string) (int64, error) {
	offset, valid := e.counterInteger(args[1])
	if !valid {
		return 0, errNotAnInteger
	}
	if offset < 0 {
		return 0, errOffsetRange
	}
	return offset, nil
}

// cmdGETDEL implements GETDEL key. The value's reply is admitted before the
// key is deleted, as SET ... GET admits the old value's before it writes: a
// reply that cannot be sent leaves the key.
func (e *Engine) cmdGETDEL(args []string) []byte {
	e.recordsItself()
	if len(args) != 1 {
		return e.encode(wrongArguments("GETDEL"), false)
	}
	r := e.reply()
	value, ok := e.opGet(args[0])
	if !ok {
		return r.Null().Bytes()
	}
	if r.Bulk(value).Refused() {
		return r.Bytes()
	}
	e.deleteString(args[0])
	return r.Bytes()
}

// cmdGETSET implements GETSET key value: SET answering the value it
// replaced, its reply admitted before anything is written. A key of another
// type is refused, as GET refuses it.
func (e *Engine) cmdGETSET(args []string) []byte {
	e.recordsItself()
	if len(args) != 2 {
		return e.encode(wrongArguments("GETSET"), false)
	}
	r := e.reply()
	if old, ok := e.opGet(args[0]); !ok {
		r.Null()
	} else if r.Bulk(old).Refused() {
		return r.Bytes()
	}
	e.storeString(args[0], args[1])
	return r.Bytes()
}

// cmdGETEX implements GETEX key [EX seconds | PX milliseconds | EXAT
// unix-time-seconds | PXAT unix-time-milliseconds | PERSIST], in Redis's
// order: the options, then the key - a missing one answers null, whatever
// its expiry says - then the expiry's value, then the reply, then the
// expiry.
func (e *Engine) cmdGETEX(args []string) []byte {
	e.recordsItself()
	if len(args) < 1 {
		return e.encode(wrongArguments("GETEX"), false)
	}
	unit, expire, persist, err := getExOptions(args[1:])
	if err != nil {
		return e.encode(err, false)
	}
	r := e.reply()
	obj := e.dictStore.Get(args[0])
	if obj == nil {
		return r.Null().Bytes()
	}
	req := getExRequest{persist: persist}
	if unit != "" {
		if req.at, err = e.expiryArgument("GETEX", unit, expire); err != nil {
			return e.encode(err, false)
		}
		req.expires, req.absolute = true, unit == "EXAT" || unit == "PXAT"
	}
	if r.Bulk(obj.Value).Refused() {
		return r.Bytes()
	}
	e.opGetExExpiry(args[0], req)
	return r.Bytes()
}

// getExOptions reads GETEX's options as Redis's
// parseExtendedStringArgumentsOrReply reads a GET's: PERSIST, or one of the
// four expiries, which may be given again, its last value counting; anything
// else - NX, XX, GET, KEEPTTL among them - is a syntax error.
func getExOptions(opts []string) (unit, expire string, persist bool, err error) {
	for i := 0; i < len(opts); i++ {
		switch opt := strings.ToUpper(opts[i]); {
		case opt == "PERSIST" && unit == "":
			persist = true
		case (opt == "EX" || opt == "PX" || opt == "EXAT" || opt == "PXAT") && !persist &&
			(unit == "" || unit == opt) && i+1 < len(opts):
			unit, expire = opt, opts[i+1]
			i++
		default:
			return "", "", false, errSyntax
		}
	}
	return unit, expire, persist, nil
}

// cmdMSETNX implements MSETNX key value [key value ...]: 1 when it stored
// every pair, 0 when any key already existed and it stored none.
func (e *Engine) cmdMSETNX(args []string) []byte {
	e.recordsItself()
	if len(args) == 0 || len(args)%2 != 0 {
		return e.encode(wrongArguments("MSETNX"), false)
	}
	r := e.reply()
	if e.opMSetNX(args) {
		return r.Int(1).Bytes()
	}
	return r.Int(0).Bytes()
}

// cmdINCRBYFLOAT implements INCRBYFLOAT key increment. The key's type is
// checked before the increment is read, as Redis checks it: an increment
// that is not a number is refused in the same words as a value that is not.
// The sum's reply is admitted before it is stored: a reply that cannot be
// sent leaves the key.
func (e *Engine) cmdINCRBYFLOAT(args []string) []byte {
	e.recordsItself()
	if len(args) != 2 {
		return e.encode(wrongArguments("INCRBYFLOAT"), false)
	}
	r := e.reply()
	by, ok := parseLongDouble(args[1])
	if !ok {
		return r.Error(errNotAFloat).Bytes()
	}
	value, exists, err := e.floatSum(args[0], by)
	if err != nil {
		return r.Error(err).Bytes()
	}
	if r.Bulk(value).Refused() {
		return r.Bytes()
	}
	e.storeFloatSum(args[0], value, exists)
	return r.Bytes()
}
