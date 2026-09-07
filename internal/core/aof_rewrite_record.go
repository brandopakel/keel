package core

import "strconv"

// A record retains immutable string references and emits at most 64 KiB each
// cycle. It must finish even if the key changes: dirty reconciliation replaces
// the completed historical record. Cancelling halfway would corrupt RESP.
type rewriteRecord struct {
	parts        []string
	part, offset int
	payload      []byte
	payloadPart  int
}

func (r *rewriteRecord) appendCommand(command ...string) {
	r.parts = append(r.parts, "*"+strconv.Itoa(len(command))+"\r\n")
	for _, part := range command {
		r.parts = append(r.parts, "$"+strconv.Itoa(len(part))+"\r\n", part, "\r\n")
	}
}

// A mutable sketch needs one immutable encoded image to finish a valid record
// across intervening mutations. Retain that image directly; converting it to a
// string and constructing another whole RESP record would triple its storage.
func appendOpaqueRewriteRecord(dst []byte, key string, reset bool, plan dumpPlan, expiry uint64) []byte {
	r := &rewriteRecord{payload: appendDump(make([]byte, 0, plan.size+9), plan)}
	if reset {
		r.appendCommand("DEL", key)
	}
	r.appendCommand("KEEL.RESTORE", key, "")
	r.payloadPart = len(r.parts) - 2
	r.parts[r.payloadPart-1] = "$" + strconv.Itoa(len(r.payload)) + "\r\n"
	if expiry > 0 {
		r.appendCommand("PEXPIREAT", key, strconv.FormatUint(expiry, 10))
	}
	rewrite.stream = r
	return emitRewriteRecordSlice(dst)
}

func appendRewriteRecords(dst []byte, commands [][]string) []byte {
	size := 0
	for _, command := range commands {
		size += 32
		for _, part := range command {
			size += len(part) + 32
		}
	}
	if size <= rewriteRecordSlice {
		for _, command := range commands {
			dst = appendCommand(dst, command...)
		}
		return dst
	}
	r := &rewriteRecord{}
	for _, command := range commands {
		r.appendCommand(command...)
	}
	rewrite.stream = r
	return emitRewriteRecordSlice(dst)
}

func emitRewriteRecordSlice(dst []byte) []byte {
	r := rewrite.stream
	budget := rewriteRecordSlice
	for r.part < len(r.parts) && budget > 0 {
		part := r.parts[r.part]
		length := len(part)
		binary := r.payload != nil && r.part == r.payloadPart
		if binary {
			length = len(r.payload)
		}
		n := min(length-r.offset, budget)
		if binary {
			dst = append(dst, r.payload[r.offset:r.offset+n]...)
		} else {
			dst = append(dst, part[r.offset:r.offset+n]...)
		}
		budget -= n
		r.offset += n
		if r.offset == length {
			if binary {
				r.payload = nil
			}
			r.parts[r.part] = ""
			r.part++
			r.offset = 0
		}
	}
	if r.part == len(r.parts) {
		rewrite.stream = nil
	}
	return dst
}
