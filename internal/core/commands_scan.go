package core

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/brandopakel/keel/internal/data_structure"
)

// scanDefaultCount is Redis's default COUNT, and means the same thing here: how
// much of the keyspace one call is allowed to look at, not how many keys it
// promises to return.
const scanDefaultCount = 10

// scanMaxCount bounds what a client can ask one call to do. COUNT is a work
// budget, and a server that lets a client set it to a billion has given away
// the pause that SCAN exists to avoid.
const scanMaxCount = 1 << 20
const scanMatchWork = 1 << 20

// cmdSCAN walks the keyspace a bounded piece at a time.
//
// KEYS builds the whole reply before sending any of it, so on a server that
// runs commands one at a time every other client waits for the largest
// keyspace anyone ever has. SCAN is the answer to that, and the guarantee it
// has to keep is that a key present for the whole walk is returned, without
// the walk holding a copy of the keyspace or pinning it against writes.
//
// The cursor encodes a stable slot and keyspace index. It holds no server
// iterator or snapshot and survives mutation of other entries. The work,
// oversized-name and matcher limits are documented in keyspace-traversal.md.
//
// COUNT bounds keys examined, so MATCH and TYPE filter what has already been
// paid for. A selective filter therefore returns short or empty batches with a
// cursor still to follow, and a client must stop on a zero cursor rather than
// on an empty reply. That is Redis's rule too.
func cmdSCAN(args []string) []byte {
	if len(args) < 1 {
		return Encode(errors.New("ERR wrong number of arguments for 'SCAN' command"), false)
	}
	cursor, err := strconv.ParseUint(args[0], 10, 64)
	if err != nil {
		// Redis says "invalid cursor" rather than the integer error, and
		// clients that retry from zero test for it.
		return Encode(errors.New("ERR invalid cursor"), false)
	}

	count := scanDefaultCount
	pattern, keyspace := "", ""
	matchSet, typeSet := false, false
	for i := 1; i < len(args); {
		switch strings.ToUpper(args[i]) {
		case "MATCH":
			if i+1 >= len(args) {
				return Encode(errSyntax, false)
			}
			pattern, i = args[i+1], i+2
			matchSet = true
		case "COUNT":
			if i+1 >= len(args) {
				return Encode(errSyntax, false)
			}
			n, convErr := strconv.Atoi(args[i+1])
			if convErr != nil {
				return Encode(errNotAnInteger, false)
			}
			if n < 1 || n > scanMaxCount {
				return Encode(errSyntax, false)
			}
			count, i = n, i+2
		case "TYPE":
			if i+1 >= len(args) {
				return Encode(errSyntax, false)
			}
			keyspace, i = strings.ToLower(args[i+1]), i+2
			typeSet = true
		default:
			return Encode(errSyntax, false)
		}
	}

	remainingMatchWork := scanMatchWork
	matchExhausted := false
	keep := func(ks data_structure.Keyspace, key string) bool {
		// Cheapest test first, and rejecting by store name skips whole
		// keyspaces rather than asking each of their keys.
		if typeSet && ks.KeyspaceName() != keyspace {
			return false
		}
		if matchSet {
			if matchExhausted {
				return false
			}
			matched, exhausted := globMatchBounded(pattern, key, &remainingMatchWork)
			matchExhausted = exhausted
			if !matched {
				return false
			}
		}
		// The slot already establishes presence. Check expiry without a
		// second key lookup or mutation; active/lazy expiry handles removal.
		at, expires := ks.GetExpiry(key)
		return !expires || at > uint64(time.Now().UnixMilli())
	}

	if !reserveCommandMemory(4*min(count, data_structure.ScanMaxWork)*16 + 8192) {
		return allocationPressure
	}
	keys, next := data_structure.ScanKeyspaces(cursor, count, keep, nil)
	if matchExhausted {
		return Encode(errors.New("ERR SCAN pattern work limit exceeded; use a simpler MATCH or smaller COUNT"), false)
	}
	position := strconv.FormatUint(next, 10)
	size := 4 + decimalDigits(len(keys)) + 3
	var fits bool
	size, fits = addBulkSize(size, len(position))
	for _, key := range keys {
		if !fits {
			break
		}
		size, fits = addBulkSize(size, len(key))
	}
	if !fits {
		return replyTooLarge
	}
	if !reserveReplyMemory(size) {
		return allocationPressure
	}
	out := appendArrayHeader(make([]byte, 0, size), 2)
	out = appendBulkString(out, position)
	out = appendArrayHeader(out, len(keys))
	for _, key := range keys {
		out = appendBulkString(out, key)
	}
	return out
}
