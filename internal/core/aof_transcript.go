package core

import (
	"fmt"
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

// writeAOFBuffer writes what is buffered to e's log; drain says it is a drain
// in the middle of a record (appendAOFFragment), and not a flush of whole
// records.
//
// A write that fails partway is first cut back off the file, as Redis 8.10.1
// truncates a short write to the last whole size (aof.c 1515-1526), when the
// file ends at a record boundary: the log is then whole, and the whole buffer
// is still to be written. When the file ends in the middle of a record, whose
// earlier drains are on disk, or when the cut fails, what was written stays,
// and only the rest is still to be written, as Redis does when it cannot
// truncate (aof.c 1547-1551).
//
// The failure then latches (aof.failed), and every later write returns it,
// unless e retries a failed write, as an engine Open makes does under everysec
// and no (driver.go): what is still to be written stays buffered, in order,
// for the maintenance goroutine's next flush to try again, as Redis keeps it
// in its aof_buf (aof.c 1539-1552). That holds for a drain in the middle of a
// large record too: what follows the failure is kept with it
// (appendAOFFragment), so the log is whole once the write succeeds.
func (e *Engine) writeAOFBuffer(drain bool) error {
	e.pollAppend(true)
	if e.aof.failed != nil {
		return e.aof.failed
	}
	if e.aof.file == nil {
		return nil
	}
	if len(e.aof.buf) > 0 {
		n, err := timedPersistenceWrite(&e.appendWriteStats, e.aof.file, e.aof.buf, e.aofWrite)
		if err == nil && n != len(e.aof.buf) {
			err = io.ErrShortWrite
		}
		if err != nil && n > 0 && !e.aof.midRecord && e.cutShortWrite(n) {
			n = 0
		}
		e.recordAOFDigest(e.aof.buf[:n])
		e.aof.written += int64(n)
		e.appendStarted += uint64(n)
		e.appendWritten += uint64(n)
		if n > 0 {
			e.aof.dirty = true
		}
		if err != nil {
			if n > 0 {
				// What was written stays, so the file now ends partway
				// through a record.
				e.aof.midRecord = true
			}
			e.aof.buf = e.aof.buf[n:]
			e.aof.commandStart = max(e.aof.commandStart-n, 0)
			if d := e.driver; d != nil && d.retries {
				d.writeFailed(err)
				return err
			}
			e.aof.failed = err
			return err
		}
		e.aof.midRecord = drain
		if d := e.driver; d != nil && d.writeFailure != nil {
			d.writeSolved()
		}
		e.publishAOFPrefix()
		if cap(e.aof.buf) > maxAOFTranscriptBytes {
			// A record kept whole past the bound while a failed write was
			// retried: once it is written, its buffer goes too.
			e.aof.buf = nil
		} else {
			e.aof.buf = e.aof.buf[:0]
		}
		e.aof.commandStart = 0
	}
	return nil
}

//go:noinline
func padA0() {}

//go:noinline
func padA1() {}

// cutShortWrite cuts the n bytes a short write left at the end of e's log
// back off it, as Redis truncates a short write (aof.c 1515-1526), and says
// whether it did. The size to cut back to is the file's own, less n, rather
// than any count kept of it: the log is opened for appending and only e
// writes it, so that is exactly where the write began, and a count that had
// drifted from the file could only make the cut remove what was there before.
func (e *Engine) cutShortWrite(n int) bool {
	info, err := e.aof.file.Stat()
	if err == nil && info.Size() >= int64(n) {
		err = e.aofTruncate(e.aof.file, info.Size()-int64(n))
	} else if err == nil {
		err = fmt.Errorf("the log is %d bytes, fewer than the %d just written", info.Size(), n)
	}
	if err != nil {
		aofLog("could not remove a short write from the log, so the rest is written after it: %v", err)
		return false
	}
	return true
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

// appendAOFFragment appends part of a record too large for what is left of the
// buffer, draining the buffer to the log whenever it fills. While a failed
// write is retried, it drains nothing: the rest of the record is kept whole,
// past the bound, as Redis keeps a record in its aof_buf, and the maintenance
// goroutine writes it, in order, when the log recovers. Writes are refused
// meanwhile, so what is kept is the record being written, and what expiry or
// eviction removes, as in Redis.
func (e *Engine) appendAOFFragment(fragment string) {
	for len(fragment) > 0 && e.aof.failed == nil {
		if e.writeRetrying() {
			e.aof.buf = append(e.aof.buf, fragment...)
			return
		}
		if len(e.aof.buf) == maxAOFTranscriptBytes {
			// A failed drain either latches, ending the loop, or is retried,
			// keeping the rest of the record (above).
			if e.writeAOFBuffer(true) != nil {
				continue
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
