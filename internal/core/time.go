package core

import (
	"strconv"
	"time"
)

// cmdTIME is Redis's TIME: the server's clock as two bulk strings, the Unix
// time in seconds and the microseconds past it (server.c, timeCommand).
func (e *Engine) cmdTIME(args []string) []byte {
	if len(args) != 0 {
		return e.encode(wrongArguments("TIME"), false)
	}
	now := time.Now()
	us := now.UnixMicro()
	out := appendArrayHeader(nil, 2)
	out = appendBulkString(out, strconv.FormatInt(us/1e6, 10))
	return appendBulkString(out, strconv.FormatInt(us%1e6, 10))
}
