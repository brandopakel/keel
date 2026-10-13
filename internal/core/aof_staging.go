package core

import "strconv"

// Staged records: what a command wants written to the log in its place.
//
// A command whose arrival would not replay to the state it left - SET with a
// relative expiry, SPOP, a write over a key of another type - stages the
// records that would, and aofCommit writes those instead of the command. The
// typed operations of phase 4 (docs/embedding-plan.md, step 4.3) stage the
// record of every write they make, because an embedded caller has no command
// to log as it arrived.
//
// Staging allocates nothing once the engine's staging area has grown to the
// size of the largest record a command stages. A record's parts are held flat
// in e.aof.staged, each record led by a header that counts its parts, so a
// record is copied into storage the engine keeps rather than kept as the
// variadic slice its caller built, which escaped to the heap on every write
// and is now on the caller's stack. A part may be an integer, written in
// decimal when the record is encoded, so that an expiry's instant or an
// increment is logged without a string being made for it. aofBegin and
// aofCommit clear the area, so it never holds a value past its command.

// stagedPart is one part of a staged record, or the header that leads one.
type stagedPart struct {
	s string
	n int64
	// kind says which of s and n the part is, or that it is a record's
	// header, whose n counts the parts that follow it.
	kind uint8
}

const (
	stagedString uint8 = iota
	stagedInt
	stagedHeader
)

// aofRecord stages one record, its parts as given, for the command being
// executed.
func (e *Engine) aofRecord(parts ...string) {
	if e.aof.file == nil || e.aof.replaying {
		return
	}
	e.aof.staged = append(e.aof.staged, stagedPart{n: int64(len(parts)), kind: stagedHeader})
	for _, p := range parts {
		e.aof.staged = append(e.aof.staged, stagedPart{s: p})
	}
}

// aofRecordInt stages the record name key n, whose last part is an integer:
// PEXPIREAT's instant, INCRBY's increment.
func (e *Engine) aofRecordInt(name, key string, n int64) {
	if e.aof.file == nil || e.aof.replaying {
		return
	}
	e.aof.staged = append(e.aof.staged, stagedPart{n: 3, kind: stagedHeader},
		stagedPart{s: name}, stagedPart{s: key}, stagedPart{n: n, kind: stagedInt})
}

// clearStaged empties the staging area, keeping its storage and dropping its
// references, so that no value a command staged outlives the command.
func (e *Engine) clearStaged() {
	clear(e.aof.staged)
	e.aof.staged = e.aof.staged[:0]
}

// appendStaged writes every staged record to the log, in the order staged.
func (e *Engine) appendStaged() {
	staged := e.aof.staged
	for i := 0; i < len(staged); {
		n := int(staged[i].n)
		e.appendAOFParts(staged[i+1 : i+1+n])
		i += 1 + n
	}
}

// partSize is how many bytes a part's text takes, written as a bulk string's
// payload.
func partSize(p stagedPart) int {
	if p.kind != stagedInt {
		return len(p.s)
	}
	if p.n < 0 {
		if p.n == -1<<63 {
			return 20
		}
		return 1 + decimalDigits(int(-p.n))
	}
	return decimalDigits(int(p.n))
}

// appendAOFParts writes one record whose parts are given as staged: the
// record appendAOFCommand writes for the same strings, an integer in the
// decimal text strconv gives it.
func (e *Engine) appendAOFParts(parts []stagedPart) {
	if e.aof.failed != nil {
		return
	}
	if e.aof.transaction && !e.aof.transactionLogged {
		e.aof.transactionLogged = true
		e.openAOFTransaction()
	}
	e.aof.commandChanged = true
	size := 1 + decimalDigits(len(parts)) + 2
	fits := true
	for _, p := range parts {
		length := partSize(p)
		overhead := 1 + decimalDigits(length) + 4
		if length > maxAOFTranscriptBytes-size-overhead {
			fits = false
			break
		}
		size += overhead + length
	}
	if fits && size <= maxAOFTranscriptBytes-len(e.aof.buf) {
		e.growAOFBuffer(size)
		e.aof.buf = appendArrayHeader(e.aof.buf, len(parts))
		for _, p := range parts {
			if p.kind == stagedInt {
				e.aof.buf = append(e.aof.buf, '$')
				e.aof.buf = strconv.AppendInt(e.aof.buf, int64(partSize(p)), 10)
				e.aof.buf = append(e.aof.buf, '\r', '\n')
				e.aof.buf = strconv.AppendInt(e.aof.buf, p.n, 10)
				e.aof.buf = append(e.aof.buf, '\r', '\n')
				continue
			}
			e.aof.buf = appendBulkString(e.aof.buf, p.s)
		}
		return
	}
	// A record too large for what is left of the buffer is drained in
	// fragments, as appendAOFCommand drains one. Only a record with a part
	// of megabytes comes here, so an integer part's text may be made.
	strs := make([]string, len(parts))
	for i, p := range parts {
		strs[i] = p.s
		if p.kind == stagedInt {
			strs[i] = strconv.FormatInt(p.n, 10)
		}
	}
	e.appendAOFCommand(strs[0], strs[1:]...)
}
