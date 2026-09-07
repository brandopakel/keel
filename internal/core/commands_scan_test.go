package core

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scanAll walks SCAN to completion the way a client must: hand the cursor back
// until it comes out zero, and stop on that rather than on an empty batch.
func scanAll(t *testing.T, options ...string) ([]string, int) {
	t.Helper()
	var keys []string
	cursor, calls := "0", 0
	for {
		reply := run(t, "SCAN", append([]string{cursor}, options...)...)
		pair, ok := reply.([]interface{})
		require.True(t, ok, "SCAN answers a two-element array, got %#v", reply)
		require.Len(t, pair, 2)

		next, ok := pair[0].(string)
		require.True(t, ok, "the cursor is a bulk string")
		keys = append(keys, toStrings(pair[1])...)

		calls++
		require.Less(t, calls, 5000, "the walk must terminate")
		if next == "0" {
			return keys, calls
		}
		cursor = next
	}
}

func TestScanReturnsEveryKeyExactlyOnce(t *testing.T) {
	ResetStores()
	want := map[string]bool{}
	for i := 0; i < 500; i++ {
		key := "cache:" + strconv.Itoa(i)
		run(t, "SET", key, "v")
		want[key] = true
	}
	// Every keyspace has to be walked, not just the string one: a name lives in
	// exactly one store and the client does not know which.
	for _, k := range []string{"theset", "thehash", "thelist", "thezset", "thehll"} {
		want[k] = true
	}
	run(t, "SADD", "theset", "m")
	run(t, "HSET", "thehash", "f", "v")
	run(t, "RPUSH", "thelist", "v")
	run(t, "ZADD", "thezset", "1", "m")
	run(t, "PFADD", "thehll", "m")

	got, calls := scanAll(t)
	assert.Greater(t, calls, 1, "505 keys must not arrive in a single call")

	seen := map[string]int{}
	for _, key := range got {
		seen[key]++
	}
	for key := range want {
		assert.Equal(t, 1, seen[key], "%s must be returned exactly once", key)
	}
	assert.Len(t, seen, len(want), "and nothing else may appear")
}

func TestScanAgreesWithKeys(t *testing.T) {
	ResetStores()
	for i := 0; i < 200; i++ {
		run(t, "SET", "k"+strconv.Itoa(i), "v")
	}
	run(t, "SADD", "s", "m")

	scanned, _ := scanAll(t)
	listed := toStrings(run(t, "KEYS", "*"))
	assert.ElementsMatch(t, listed, scanned, "SCAN and KEYS must see the same keyspace")
}

func TestScanMatchFiltersWithoutLosingTheWalk(t *testing.T) {
	ResetStores()
	for i := 0; i < 300; i++ {
		run(t, "SET", "user:"+strconv.Itoa(i), "v")
		run(t, "SET", "session:"+strconv.Itoa(i), "v")
	}
	got, _ := scanAll(t, "MATCH", "user:*")
	assert.Len(t, got, 300, "a filtered walk still reaches every matching key")
	for _, key := range got {
		assert.Contains(t, key, "user:")
	}
}

func TestScanTypeSelectsOneKeyspace(t *testing.T) {
	ResetStores()
	run(t, "SET", "a-string", "v")
	run(t, "SADD", "a-set", "m")
	run(t, "HSET", "a-hash", "f", "v")

	assert.Equal(t, []string{"a-set"}, mustScan(t, "TYPE", "set"))
	assert.Equal(t, []string{"a-hash"}, mustScan(t, "TYPE", "hash"))
	assert.Equal(t, []string{"a-string"}, mustScan(t, "TYPE", "string"))
	assert.Empty(t, mustScan(t, "TYPE", "nosuchtype"), "an unknown type matches nothing")
}

func TestScanExplicitEmptyAndCaseInsensitiveFilters(t *testing.T) {
	ResetStores()
	run(t, "SET", "", "empty-name")
	run(t, "SET", "ordinary", "v")
	run(t, "SADD", "members", "v")
	assert.Equal(t, []string{""}, mustScan(t, "MATCH", ""))
	assert.Empty(t, mustScan(t, "TYPE", ""))
	assert.ElementsMatch(t, []string{"", "ordinary"}, mustScan(t, "TYPE", "STRING"))
	assert.Equal(t, []string{"members"}, mustScan(t, "TYPE", "SeT"))
	assert.Empty(t, mustScan(t, "MATCH", "", "TYPE", "set"))
}

func mustScan(t *testing.T, options ...string) []string {
	t.Helper()
	keys, _ := scanAll(t, options...)
	return keys
}

// COUNT is a work budget, so a selective filter is allowed to answer with an
// empty batch and a cursor still to follow. A client that stopped on the empty
// batch would miss most of the keyspace, which is why the contract is to stop
// on the zero cursor instead.
func TestScanCountBoundsWorkNotResults(t *testing.T) {
	ResetStores()
	for i := 0; i < 2000; i++ {
		run(t, "SET", "k"+strconv.Itoa(i), "v")
	}
	run(t, "SET", "needle", "v")

	found, calls, cursor := 0, 0, "0"
	empties := 0
	for {
		reply := run(t, "SCAN", cursor, "MATCH", "needle", "COUNT", "10")
		pair := reply.([]interface{})
		next := pair[0].(string)
		batch := toStrings(pair[1])
		if len(batch) == 0 {
			empties++
		}
		found += len(batch)
		calls++
		require.Less(t, calls, 5000)
		if next == "0" {
			break
		}
		cursor = next
	}
	assert.Equal(t, 1, found, "the one matching key is found")
	assert.Greater(t, empties, 0, "and most batches along the way were empty")
}

func TestScanDoesNotShowExpiredKeys(t *testing.T) {
	ResetStores()
	run(t, "SET", "live", "v")
	run(t, "SET", "gone", "v")
	// An absolute expiry in the past, set on the store directly: going through
	// EXPIREAT would delete the key outright rather than leave it present and
	// due, which is the state this test is about.
	dictStore.SetExpiryAt("gone", 1)

	got, _ := scanAll(t)
	assert.Contains(t, got, "live")
	assert.NotContains(t, got, "gone", "SCAN must not show a key GET would say was gone")
}

func TestScanRejectsBadArguments(t *testing.T) {
	ResetStores()
	assert.Equal(t, "ERR invalid cursor", run(t, "SCAN", "notanumber"))
	assert.Equal(t, "ERR invalid cursor", run(t, "SCAN", "-1"))
	assert.Contains(t, run(t, "SCAN").(string), "wrong number of arguments")
	assert.Equal(t, "ERR syntax error", run(t, "SCAN", "0", "NOSUCHOPTION"))
	assert.Equal(t, "ERR syntax error", run(t, "SCAN", "0", "MATCH"))
	assert.Equal(t, "ERR syntax error", run(t, "SCAN", "0", "COUNT"))
	assert.Equal(t, "ERR syntax error", run(t, "SCAN", "0", "TYPE"))
	assert.Equal(t, "ERR syntax error", run(t, "SCAN", "0", "COUNT", "0"),
		"a zero budget would make no progress")
	assert.Equal(t, errNotAnInteger.Error(), run(t, "SCAN", "0", "COUNT", "many"))
}

// A cursor past the end of the keyspace reports the walk as finished rather
// than reading the wrong store, so a stale cursor cannot make SCAN lie.
func TestScanTreatsAStaleCursorAsFinished(t *testing.T) {
	ResetStores()
	run(t, "SET", "k", "v")
	reply := run(t, "SCAN", "999999999").([]interface{})
	assert.Equal(t, "0", reply[0])
	assert.Empty(t, toStrings(reply[1]))
}

func TestScanOnAnEmptyKeyspaceFinishesImmediately(t *testing.T) {
	ResetStores()
	keys, calls := scanAll(t)
	assert.Empty(t, keys)
	assert.Equal(t, 1, calls, "an empty server must answer in one call")
}

func TestScanPatternExhaustionIsAnErrorNotAnEmptyMatch(t *testing.T) {
	ResetStores()
	run(t, "SET", strings.Repeat("a", 10000), "v")
	reply := run(t, "SCAN", "0", "MATCH", "*"+strings.Repeat("a", 1000)+"b")
	assert.Contains(t, reply, "ERR SCAN pattern work limit exceeded")
	assert.Len(t, mustScan(t, "MATCH", "a*"), 1)
}
