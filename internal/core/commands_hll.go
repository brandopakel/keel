package core

import "github.com/brandopakel/keel/internal/constant"

// HyperLogLog commands, following Redis semantics: the command language over
// the typed operations of hll.go. Each reads its arguments, calls its
// operation, and answers through the engine's reply sink; a write records
// itself, and is never logged as it arrived.

func (e *Engine) cmdPFADD(args []string) []byte {
	e.recordsItself()
	if len(args) < 1 {
		return e.encode(wrongArguments("PFADD"), false)
	}
	// 1 and 0 are shared constants, answered by reference with no Reply to
	// build (as GET answers its one value).
	if e.opPFAdd(args[0], args[1:]) {
		return constant.RespOne
	}
	return constant.RespZero
}

func (e *Engine) cmdPFCOUNT(args []string) []byte {
	if len(args) < 1 {
		return e.encode(wrongArguments("PFCOUNT"), false)
	}
	r := e.reply()
	return r.Int(e.opPFCount(args)).Bytes()
}

func (e *Engine) cmdPFMERGE(args []string) []byte {
	e.recordsItself()
	if len(args) < 1 {
		return e.encode(wrongArguments("PFMERGE"), false)
	}
	e.opPFMerge(args[0], args[1:])
	r := e.reply()
	return r.Status("OK").Bytes()
}
