package core

import (
	"io"
	"strconv"
)

// maxAOFTranscriptBytes bounds the encoded command/expiry/eviction buffer.
// A large command borrows its existing strings and drains fragments in order.
// Drains can block on storage; they neither sync nor advance a rewrite. Only
// the normal completed-command flush may acknowledge or replace the AOF.
// Leave framing headroom above four one-MiB payloads. An exact power-of-two
// ceiling otherwise rejects the fourth ordinary record solely for its header.
const maxAOFTranscriptBytes = (4 << 20) + (64 << 10)

// writeAOFBuffer writes what is buffered to e's log. A failed write latches
// (aof.failed), and every later write returns it, unless the buffer holds
// whole records and e retries a failed write, as an engine Open makes does
// under everysec and no (driver.go): then what was not written stays
// buffered for the next flush to try again, as Redis keeps it. A drain in the
// middle of a large record cannot keep the record whole, so it always latches.
func (e *Engine) writeAOFBuffer(wholeRecords bool) error {
	e.pollAppend(true)
	if e.aof.failed != nil {
		return e.aof.failed
	}
	if e.aof.file == nil {
		return nil
	}
	if len(e.aof.buf) > 0 {
		n, err := timedPersistenceWrite(&e.appendWriteStats, e.aof.file, e.aof.buf, e.aofWrite)
		e.recordAOFDigest(e.aof.buf[:n])
		e.aof.written += int64(n)
		e.appendStarted += uint64(n)
		e.appendWritten += uint64(n)
		if n > 0 {
			e.aof.dirty = true
		}
		if err == nil && n != len(e.aof.buf) {
			err = io.ErrShortWrite
		}
		if err != nil {
			e.aof.buf = e.aof.buf[n:]
			if d := e.driver; wholeRecords && d != nil && d.retries {
				d.writeFailed(err)
				return err
			}
			e.aof.failed = err
			return err
		}
		if d := e.driver; d != nil && d.writeFailure != nil {
			d.writeSolved()
		}
		e.publishAOFPrefix()
		e.aof.buf = e.aof.buf[:0]
		e.aof.commandStart = 0
	}
	return nil
}

func (e *Engine) publishAOFPrefix() {
	if e.aof.commandActive && !e.aof.commandOpaque && e.aof.commandStart < len(e.aof.buf) {
		e.recordReplicationV2Body(e.aof.buf[e.aof.commandStart:])
		e.aof.commandStart = len(e.aof.buf)
	}
}

// growAOFBuffer bounds backing capacity as well as length; unconstrained append
// growth could retain more than the transcript ceiling even for small records.
func (e *Engine) growAOFBuffer(size int) {
	if cap(e.aof.buf)-len(e.aof.buf) >= size {
		return
	}
	capacity := min(maxAOFTranscriptBytes, max(len(e.aof.buf)+size, max(4096, 2*cap(e.aof.buf))))
	// Include the small framing allowance in the final geometric growth;
	// growing to exactly four MiB and then again for headers wastes a buffer.
	if capacity >= 4<<20 {
		capacity = maxAOFTranscriptBytes
	}
	buf := make([]byte, len(e.aof.buf), capacity)
	copy(buf, e.aof.buf)
	e.aof.buf = buf
}

func (e *Engine) appendAOFFragment(fragment string) {
	for len(fragment) > 0 && e.aof.failed == nil {
		if len(e.aof.buf) == maxAOFTranscriptBytes {
			if e.writeAOFBuffer(false) != nil {
				return
			}
		}
		n := min(len(fragment), maxAOFTranscriptBytes-len(e.aof.buf))
		e.growAOFBuffer(n)
		e.aof.buf = append(e.aof.buf, fragment[:n]...)
		fragment = fragment[n:]
	}
}

func (e *Engine) appendAOFCommand(name string, args ...string) {
	if e.aof.failed != nil {
		return
	}
	if e.aof.transaction && !e.aof.transactionLogged {
		e.aof.transactionLogged = true
		e.openAOFTransaction()
	}
	e.aof.commandChanged = true
	// Most commands stay on the same coalesced fast path. Bound the size walk
	// by subtraction so even an enormous argument list cannot overflow it.
	size := 1 + decimalDigits(len(args)+1) + 2
	fits := true
	add := func(field string) {
		overhead := 1 + decimalDigits(len(field)) + 4
		if len(field) > maxAOFTranscriptBytes-size-overhead {
			fits = false
			return
		}
		size += overhead + len(field)
	}
	add(name)
	for _, arg := range args {
		if !fits {
			break
		}
		add(arg)
	}
	if fits && size <= maxAOFTranscriptBytes-len(e.aof.buf) {
		e.growAOFBuffer(size)
		e.aof.buf = appendArrayHeader(e.aof.buf, len(args)+1)
		e.aof.buf = appendBulkString(e.aof.buf, name)
		for _, arg := range args {
			e.aof.buf = appendBulkString(e.aof.buf, arg)
		}
		return
	}
	var header [32]byte
	e.appendAOFFragment(string(appendArrayHeader(header[:0], len(args)+1)))
	field := func(value string) {
		bulkHeader := strconv.AppendInt(append(header[:0], '$'), int64(len(value)), 10)
		e.appendAOFFragment(string(append(bulkHeader, '\r', '\n')))
		e.appendAOFFragment(value)
		e.appendAOFFragment("\r\n")
	}
	field(name)
	for _, arg := range args {
		field(arg)
		if e.aof.failed != nil {
			return
		}
	}
}
