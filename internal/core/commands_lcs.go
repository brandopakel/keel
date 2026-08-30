package core

import (
	"errors"
	"strconv"
	"strings"

	"memkv/internal/constant"
	"memkv/internal/data_structure"
)

// LCS key1 key2 [LEN] [IDX] [MINMATCHLEN len] [WITHMATCHLEN]
//
// The one command here that is not a lookup: it computes, and what it computes
// costs the product of the two value lengths. Everything else in this server
// answers in time proportional to one key; LCS is the exception, and on a
// single-threaded server that makes it the one command a client can use to hold
// up every other client. Hence the guard, and hence the fact that the limit is
// an operator setting rather than a constant - see config.LCSMaxCells.

func cmdLCS(args []string) []byte {
	if len(args) < 2 {
		return Encode(errors.New("(error) ERR wrong number of arguments for 'LCS' command"), false)
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
			n, err := strconv.ParseInt(args[i+1], 10, 64)
			if err != nil {
				return Encode(errors.New("ERR value is not an integer or out of range"), false)
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

	a, err := lcsValue(args[0])
	if err != nil {
		return Encode(err, false)
	}
	b, err := lcsValue(args[1])
	if err != nil {
		return Encode(err, false)
	}

	if data_structure.LCSTooLarge(a, b) {
		// Redis's message, so a client that already handles this from Redis
		// handles it here. The reason underneath differs - Redis runs out of
		// room for its table, this runs out of time budget - but the client's
		// options are the same either way.
		return Encode(errors.New("ERR String too long for LCS"), false)
	}

	if wantLen {
		// Only the length is wanted, so nothing has to be recovered and the
		// two-row form does half the work.
		return Encode(int64(data_structure.LCSLen(a, b)), false)
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
	// filter is about what is worth looking at, not about what was found.
	return Encode([]interface{}{"matches", out, "len", len(seq)}, false)
}

// lcsValue reads a key as a string, treating a missing key as empty.
//
// Redis compares against an empty string for a key that is not there, which
// makes LCS of a key and a missing one return nothing rather than an error -
// the answer is genuinely "they have nothing in common".
//
// A key holding some other type reads as absent here where Redis would answer
// WRONGTYPE, because each type has its own map and a set key is simply not in
// the string dictionary. That is what GET already does with the same key, so
// LCS is consistent with the rest of the server rather than with Redis; making
// it right needs a key directory shared across the keyspaces, which is a change
// to the keyspace and not to this command.
func lcsValue(key string) (string, error) {
	obj := dictStore.Get(key)
	if obj == nil {
		return "", nil
	}
	if dictStore.HasExpired(key) {
		return "", nil
	}
	if err := assertType(obj.TypeEncoding, constant.ObjTypeString); err != nil {
		return "", err
	}
	s, ok := obj.Value.(string)
	if !ok {
		return "", errors.New("WRONGTYPE Operation against a key holding the wrong kind of value")
	}
	return s, nil
}
