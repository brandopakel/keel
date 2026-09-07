package core

import (
	"errors"
	"strconv"
	"strings"

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

// cmdSCAN walks the keyspace a bounded piece at a time.
//
// KEYS builds the whole reply before sending any of it, so on a server that
// runs commands one at a time every other client waits for the largest
// keyspace anyone ever has. SCAN is the answer to that, and the guarantee it
// has to keep is that a key present for the whole walk is returned, without
// the walk holding a copy of the keyspace or pinning it against writes.
//
// The cursor is a shard index rather than an offset. What that does and does
// not promise is set out in data_structure/sharded.go; the part that matters
// here is that it is a small integer, so the server keeps no per-cursor state
// and a client that abandons a walk costs nothing.
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
	for i := 1; i < len(args); {
		switch strings.ToUpper(args[i]) {
		case "MATCH":
			if i+1 >= len(args) {
				return Encode(errSyntax, false)
			}
			pattern, i = args[i+1], i+2
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
			keyspace, i = args[i+1], i+2
		default:
			return Encode(errSyntax, false)
		}
	}

	keep := func(ks data_structure.Keyspace, key string) bool {
		// Cheapest test first, and rejecting by store name skips whole
		// keyspaces rather than asking each of their keys.
		if keyspace != "" && ks.KeyspaceName() != keyspace {
			return false
		}
		if pattern != "" && !globMatch(pattern, key) {
			return false
		}
		// Has settles a key whose TTL has passed, and reaps it on the way, so
		// SCAN never shows a key GET would say was gone. KEYS filters the same
		// way, for the same reason.
		return ks.Has(key)
	}

	keys, next := data_structure.ScanKeyspaces(cursor, count, keep, nil)
	if keys == nil {
		keys = []string{}
	}
	// A two-element array of the cursor and the batch. The cursor is a bulk
	// string, not an integer: it is opaque to the client, which hands back
	// whatever it was given.
	return Encode([]interface{}{strconv.FormatUint(next, 10), keys}, false)
}
