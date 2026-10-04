package core

import (
	"errors"
	"strings"

	"github.com/brandopakel/keel/internal/data_structure"
)

// LCS key1 key2 [LEN] [IDX] [MINMATCHLEN len] [WITHMATCHLEN]
//
// The one command here that is not a lookup: it computes, and what it computes
// costs the product of the two value lengths. Everything else in this server
// answers in time proportional to one key; LCS is the exception, and on a
// single-threaded server that makes it the one command a client can use to hold
// up every other client. Hence the guard, and hence the fact that the limit is
// an operator setting rather than a constant - see config.LCSMaxCells.

func (e *Engine) cmdLCS(args []string) []byte {
	if len(args) < 2 {
		return Encode(wrongArguments("LCS"), false)
	}

	var (
		wantLen      bool
		wantIdx      bool
		withMatchLen bool
		minMatchLen  int64
	)
	for i := 2; i < len(args); i++ {
		switch strings.ToUpper(args[i]) {
		case "LEN":
			wantLen = true
		case "IDX":
			wantIdx = true
		case "WITHMATCHLEN":
			withMatchLen = true
		case "MINMATCHLEN":
			if i+1 >= len(args) {
				return Encode(errors.New("ERR syntax error"), false)
			}
			n, valid := counterInteger(args[i+1])
			if !valid {
				return Encode(errNotAnInteger, false)
			}
			minMatchLen = n
			i++
		default:
			return Encode(errors.New("ERR syntax error"), false)
		}
	}
	if wantLen && wantIdx {
		// Redis's wording. The two are not contradictory - IDX already carries
		// the length - so it points that out rather than just refusing.
		return Encode(errors.New("ERR If you want both the length and indexes, please just use IDX."), false)
	}

	a, err := e.lcsValue(args[0])
	if err != nil {
		return Encode(err, false)
	}
	b, err := e.lcsValue(args[1])
	if err != nil {
		return Encode(err, false)
	}

	if e.space.LCSTooLarge(a, b) {
		// Redis's message, so a client that already handles this from Redis
		// handles it here. The reason underneath differs - Redis runs out of
		// room for its table, this runs out of time budget - but the client's
		// options are the same either way.
		return Encode(errors.New("ERR String too long for LCS"), false)
	}

	if wantLen {
		if !reserveCommandMemory(8*(min(len(a), len(b))+1) + 8192) {
			return allocationPressure
		}
		// Only the length is wanted, so nothing has to be recovered and the
		// two-row form does half the work.
		return Encode(int64(data_structure.LCSLen(a, b)), false)
	}

	// The checkpointed rows, pair/run slice growth, the subsequence and the
	// encoded output coexist. IDX also builds nested arrays.
	perByte := 128
	if wantIdx {
		perByte = 1024
	}
	shorter := min(len(a), len(b))
	maxInt := int(^uint(0) >> 1)
	base := data_structure.LCSWorkspaceBytes(len(a), len(b)) + 16384
	if shorter == 0 {
		base = 64
	}
	if shorter > (maxInt-base)/perByte || !reserveCommandMemory(base+perByte*shorter) {
		return allocationPressure
	}
	matches, seq := data_structure.LCSMatches(a, b)
	if !wantIdx {
		return Encode(seq, false)
	}

	// Reported last match first, the order Redis produces by walking back from
	// the end of both strings.
	out := make([]interface{}, 0, len(matches))
	for k := len(matches) - 1; k >= 0; k-- {
		m := matches[k]
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
	return Encode(ReplyMap{"matches", out, "len", len(seq)}, false)
}

// lcsValue reads a key as a string, treating a missing key as empty.
//
// Redis compares against an empty string for a key that is not there, which
// makes LCS of a key and a missing one return nothing rather than an error -
// the answer is genuinely "they have nothing in common".
//
// A key holding some other type never reaches here: the type check refuses
// it first, in the words Redis's LCS uses - see typeError.
func (e *Engine) lcsValue(key string) (string, error) {
	obj := e.dictStore.Get(key)
	if obj == nil {
		return "", nil
	}
	if e.dictStore.HasExpired(key) {
		return "", nil
	}
	return obj.Value, nil
}
