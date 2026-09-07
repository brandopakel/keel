package core

import "strconv"

// A record retains immutable string references and emits at most 64 KiB each
// cycle. It must finish even if the key changes: dirty reconciliation replaces
// the completed historical record. Cancelling halfway would corrupt RESP.
type rewriteRecord struct {
	parts        []string
	part, offset int
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
		r.parts = append(r.parts, "*"+strconv.Itoa(len(command))+"\r\n")
		for _, part := range command {
			r.parts = append(r.parts, "$"+strconv.Itoa(len(part))+"\r\n", part, "\r\n")
		}
	}
	rewrite.stream = r
	return emitRewriteRecordSlice(dst)
}

func emitRewriteRecordSlice(dst []byte) []byte {
	r := rewrite.stream
	budget := rewriteRecordSlice
	for r.part < len(r.parts) && budget > 0 {
		part := r.parts[r.part]
		n := min(len(part)-r.offset, budget)
		dst = append(dst, part[r.offset:r.offset+n]...)
		budget -= n
		r.offset += n
		if r.offset == len(part) {
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
