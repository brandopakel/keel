package core

import (
	"io"
	"strconv"
)

// maxAOFTranscriptBytes bounds the encoded command/expiry/eviction buffer.
// A large command borrows its existing strings and drains fragments in order.
// Drains can block on storage; they neither sync nor advance a rewrite. Only
// the normal completed-command flush may acknowledge or replace the AOF.
const maxAOFTranscriptBytes = 4 << 20

func writeAOFBuffer() error {
	pollAppend(true)
	if aof.failed != nil {
		return aof.failed
	}
	if aof.file == nil {
		return nil
	}
	if len(aof.buf) > 0 {
		n, err := timedPersistenceWrite(&appendWriteStats, aof.file, aof.buf, aofWrite)
		recordAOFDigest(aof.buf[:n])
		aof.written += int64(n)
		appendStarted += uint64(n)
		appendWritten += uint64(n)
		if n > 0 {
			aof.dirty = true
		}
		if err == nil && n != len(aof.buf) {
			err = io.ErrShortWrite
		}
		if err != nil {
			aof.buf = aof.buf[n:]
			aof.failed = err
			return err
		}
		publishAOFPrefix()
		aof.buf = aof.buf[:0]
		aof.commandStart = 0
	}
	return nil
}

func publishAOFPrefix() {
	if aof.commandActive && !aof.commandOpaque && aof.commandStart < len(aof.buf) {
		recordReplicationV2Body(aof.buf[aof.commandStart:])
		aof.commandStart = len(aof.buf)
	}
}

// growAOFBuffer bounds backing capacity as well as length; unconstrained append
// growth could retain more than the transcript ceiling even for small records.
func growAOFBuffer(size int) {
	if cap(aof.buf)-len(aof.buf) >= size {
		return
	}
	capacity := min(maxAOFTranscriptBytes, max(len(aof.buf)+size, max(4096, 2*cap(aof.buf))))
	buf := make([]byte, len(aof.buf), capacity)
	copy(buf, aof.buf)
	aof.buf = buf
}

func appendAOFFragment(fragment string) {
	for len(fragment) > 0 && aof.failed == nil {
		if len(aof.buf) == maxAOFTranscriptBytes {
			if writeAOFBuffer() != nil {
				return
			}
		}
		n := min(len(fragment), maxAOFTranscriptBytes-len(aof.buf))
		growAOFBuffer(n)
		aof.buf = append(aof.buf, fragment[:n]...)
		fragment = fragment[n:]
	}
}

func appendAOFCommand(name string, args ...string) {
	if aof.failed != nil {
		return
	}
	aof.commandChanged = true
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
	if fits && size <= maxAOFTranscriptBytes-len(aof.buf) {
		growAOFBuffer(size)
		aof.buf = appendArrayHeader(aof.buf, len(args)+1)
		aof.buf = appendBulkString(aof.buf, name)
		for _, arg := range args {
			aof.buf = appendBulkString(aof.buf, arg)
		}
		return
	}
	var header [32]byte
	appendAOFFragment(string(appendArrayHeader(header[:0], len(args)+1)))
	field := func(value string) {
		bulkHeader := strconv.AppendInt(append(header[:0], '$'), int64(len(value)), 10)
		appendAOFFragment(string(append(bulkHeader, '\r', '\n')))
		appendAOFFragment(value)
		appendAOFFragment("\r\n")
	}
	field(name)
	for _, arg := range args {
		field(arg)
		if aof.failed != nil {
			return
		}
	}
}
