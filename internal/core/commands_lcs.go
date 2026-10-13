package core

import (
	"errors"
	"strings"
)

// LCS key1 key2 [LEN] [IDX] [MINMATCHLEN len] [WITHMATCHLEN]
//
// The one command here that is not a lookup: it computes, and what it computes
// costs the product of the two value lengths. Everything else in this server
// answers in time proportional to one key; LCS is the exception, and on a
// single-threaded server that makes it the one command a client can use to hold
// up every other client. Hence the guard, and hence the fact that the limit is
// an operator setting rather than a constant - see Options.LCSMaxCells.
//
// This is LCS's command language: it reads the options, in Redis's order,
// and answers what opLCS (strings.go) finds. Its replies are built as they
// always were, without reply admission: the workspace opLCS reserves covers
// the reply, and the subsequence is bounded by the table, far below the
// output limit.

var errLCSLenWithIdx = errors.New("ERR If you want both the length and indexes, please just use IDX.")

func (e *Engine) cmdLCS(args []string) []byte {
	if len(args) < 2 {
		return e.encode(wrongArguments("LCS"), false)
	}

	var (
		req          lcsRequest
		withMatchLen bool
		minMatchLen  int64
	)
	for i := 2; i < len(args); i++ {
		switch strings.ToUpper(args[i]) {
		case "LEN":
			req.lengthOnly = true
		case "IDX":
			req.ranges = true
		case "WITHMATCHLEN":
			withMatchLen = true
		case "MINMATCHLEN":
			if i+1 >= len(args) {
				return e.encode(errSyntax, false)
			}
			n, valid := e.counterInteger(args[i+1])
			if !valid {
				return e.encode(errNotAnInteger, false)
			}
			minMatchLen = n
			i++
		default:
			return e.encode(errSyntax, false)
		}
	}
	if req.lengthOnly && req.ranges {
		// Redis's wording. The two are not contradictory - IDX already carries
		// the length - so it points that out rather than just refusing.
		return e.encode(errLCSLenWithIdx, false)
	}

	found, err := e.opLCS(args[0], args[1], req)
	switch {
	case err != nil:
		return e.encode(err, false)
	case req.lengthOnly:
		return e.encode(found.length, false)
	case !req.ranges:
		return e.encode(found.sequence, false)
	}

	// Reported last match first, the order Redis produces by walking back from
	// the end of both strings.
	out := make([]interface{}, 0, len(found.matches))
	for k := len(found.matches) - 1; k >= 0; k-- {
		m := found.matches[k]
		if int64(m.Len()) < minMatchLen {
			continue
		}
		entry := []interface{}{
			[]interface{}{m.AStart, m.AEnd},
			[]interface{}{m.BStart, m.BEnd},
		}
		if withMatchLen {
			entry = append(entry, m.Len())
		}
		out = append(out, entry)
	}

	// MINMATCHLEN filters which ranges are listed but not the reported length,
	// which stays the length of the whole subsequence. Redis does the same: the
	// filter is about what is worth looking at, not about what was found. The
	// two are a map, flattened in RESP2.
	return e.encode(ReplyMap{"matches", out, "len", len(found.sequence)}, false)
}
