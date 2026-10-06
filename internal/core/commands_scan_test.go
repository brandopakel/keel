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
func scanAll(t *testing.T, e *Engine, options ...string) ([]string, int) {
	t.Helper()
	var keys []string
	cursor, calls := "0", 0
	for {
		reply := runOn(t, e, "SCAN", append([]string{cursor}, options...)...)
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
	t.Parallel()
	e := newTestEngine(t, Options{})
	want := map[string]bool{}
	for i := 0; i < 500; i++ {
		key := "cache:" + strconv.Itoa(i)
		runOn(t, e, "SET", key, "v")
		want[key] = true
	}
	// Every keyspace has to be walked, not just the string one: a name lives in
	// exactly one store and the client does not know which.
	for _, k := range []string{"theset", "thehash", "thelist", "thezset", "thehll"} {
		want[k] = true
	}
	runOn(t, e, "SADD", "theset", "m")
	runOn(t, e, "HSET", "thehash", "f", "v")
	runOn(t, e, "RPUSH", "thelist", "v")
	runOn(t, e, "ZADD", "thezset", "1", "m")
	runOn(t, e, "PFADD", "thehll", "m")

	got, calls := scanAll(t, e)
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
	t.Parallel()
	e := newTestEngine(t, Options{})
	for i := 0; i < 200; i++ {
		runOn(t, e, "SET", "k"+strconv.Itoa(i), "v")
	}
	runOn(t, e, "SADD", "s", "m")

	scanned, _ := scanAll(t, e)
	listed := toStrings(runOn(t, e, "KEYS", "*"))
	assert.ElementsMatch(t, listed, scanned, "SCAN and KEYS must see the same keyspace")
}

func TestScanMatchFiltersWithoutLosingTheWalk(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	for i := 0; i < 300; i++ {
		runOn(t, e, "SET", "user:"+strconv.Itoa(i), "v")
		runOn(t, e, "SET", "session:"+strconv.Itoa(i), "v")
	}
	got, _ := scanAll(t, e, "MATCH", "user:*")
	assert.Len(t, got, 300, "a filtered walk still reaches every matching key")
	for _, key := range got {
		assert.Contains(t, key, "user:")
	}
}

func TestScanTypeSelectsOneKeyspace(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "SET", "a-string", "v")
	runOn(t, e, "SADD", "a-set", "m")
	runOn(t, e, "HSET", "a-hash", "f", "v")

	assert.Equal(t, []string{"a-set"}, mustScan(t, e, "TYPE", "set"))
	assert.Equal(t, []string{"a-hash"}, mustScan(t, e, "TYPE", "hash"))
	assert.Equal(t, []string{"a-string"}, mustScan(t, e, "TYPE", "string"))
	assert.Empty(t, mustScan(t, e, "TYPE", "nosuchtype"), "an unknown type matches nothing")
}

func TestScanExplicitEmptyAndCaseInsensitiveFilters(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "SET", "", "empty-name")
	runOn(t, e, "SET", "ordinary", "v")
	runOn(t, e, "SADD", "members", "v")
	assert.Equal(t, []string{""}, mustScan(t, e, "MATCH", ""))
	assert.Empty(t, mustScan(t, e, "TYPE", ""))
	assert.ElementsMatch(t, []string{"", "ordinary"}, mustScan(t, e, "TYPE", "STRING"))
	assert.Equal(t, []string{"members"}, mustScan(t, e, "TYPE", "SeT"))
	assert.Empty(t, mustScan(t, e, "MATCH", "", "TYPE", "set"))
}

func mustScan(t *testing.T, e *Engine, options ...string) []string {
	t.Helper()
	keys, _ := scanAll(t, e, options...)
	return keys
}

// COUNT is a work budget, so a selective filter is allowed to answer with an
// empty batch and a cursor still to follow. A client that stopped on the empty
// batch would miss most of the keyspace, which is why the contract is to stop
// on the zero cursor instead.
func TestScanCountBoundsWorkNotResults(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	for i := 0; i < 2000; i++ {
		runOn(t, e, "SET", "k"+strconv.Itoa(i), "v")
	}
	runOn(t, e, "SET", "needle", "v")

	found, calls, cursor := 0, 0, "0"
	empties := 0
	for {
		reply := runOn(t, e, "SCAN", cursor, "MATCH", "needle", "COUNT", "10")
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
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "SET", "live", "v")
	runOn(t, e, "SET", "gone", "v")
	// An absolute expiry in the past, set on the store directly: going through
	// EXPIREAT would delete the key outright rather than leave it present and
	// due, which is the state this test is about.
	e.dictStore.SetExpiryAt("gone", 1)

	got, _ := scanAll(t, e)
	assert.Contains(t, got, "live")
	assert.NotContains(t, got, "gone", "SCAN must not show a key GET would say was gone")
}

func TestScanRejectsBadArguments(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.Equal(t, "ERR invalid cursor", runOn(t, e, "SCAN", "notanumber"))
	assert.Equal(t, "ERR invalid cursor", runOn(t, e, "SCAN", "-1"))
	assert.Contains(t, runOn(t, e, "SCAN").(string), "wrong number of arguments")
	assert.Equal(t, "ERR syntax error", runOn(t, e, "SCAN", "0", "NOSUCHOPTION"))
	assert.Equal(t, "ERR syntax error", runOn(t, e, "SCAN", "0", "MATCH"))
	assert.Equal(t, "ERR syntax error", runOn(t, e, "SCAN", "0", "COUNT"))
	assert.Equal(t, "ERR syntax error", runOn(t, e, "SCAN", "0", "TYPE"))
	assert.Equal(t, "ERR syntax error", runOn(t, e, "SCAN", "0", "COUNT", "0"),
		"a zero budget would make no progress")
	assert.Equal(t, errNotAnInteger.Error(), runOn(t, e, "SCAN", "0", "COUNT", "many"))
}

// A cursor past the end of the keyspace reports the walk as finished rather
// than reading the wrong store, so a stale cursor cannot make SCAN lie.
func TestScanTreatsAStaleCursorAsFinished(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "SET", "k", "v")
	reply := runOn(t, e, "SCAN", "999999999").([]interface{})
	assert.Equal(t, "0", reply[0])
	assert.Empty(t, toStrings(reply[1]))
}

func TestScanOnAnEmptyKeyspaceFinishesImmediately(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	keys, calls := scanAll(t, e)
	assert.Empty(t, keys)
	assert.Equal(t, 1, calls, "an empty server must answer in one call")
}

func TestScanPatternExhaustionIsAnErrorNotAnEmptyMatch(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "SET", strings.Repeat("a", 10000), "v")
	reply := runOn(t, e, "SCAN", "0", "MATCH", "*"+strings.Repeat("a", 1000)+"b")
	assert.Contains(t, reply, "ERR SCAN pattern work limit exceeded")
	assert.Len(t, mustScan(t, e, "MATCH", "a*"), 1)
}
