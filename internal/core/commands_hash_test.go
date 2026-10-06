package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHSetCountsNewFieldsOnly(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.Equal(t, int64(2), runOn(t, e, "HSET", "h", "a", "1", "b", "2"))
	assert.Equal(t, int64(0), runOn(t, e, "HSET", "h", "a", "changed"),
		"an overwrite is not a new field, even when the value changes")
	assert.Equal(t, int64(1), runOn(t, e, "HSET", "h", "c", "3"))
	assert.Equal(t, "changed", runOn(t, e, "HGET", "h", "a"))
	assert.Equal(t, int64(3), runOn(t, e, "HLEN", "h"))
}

func TestHSetRefusesAnIncompletePair(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	res, _ := runOn(t, e, "HSET", "h", "a", "1", "b").(string)
	assert.Contains(t, res, "wrong number of arguments")
	assert.Equal(t, int64(0), runOn(t, e, "EXISTS", "h"), "and creates nothing")
}

func TestHGetAndHMGetOnMissingThings(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "HSET", "h", "a", "1")

	assert.Equal(t, "$-1\r\n", string(rawReplyOn(t, e, "HGET", "h", "absent")),
		"a missing field is a null, not an empty string")
	assert.Equal(t, "$-1\r\n", string(rawReplyOn(t, e, "HGET", "absent", "a")),
		"and so is a missing key")
	assert.Equal(t, "*2\r\n$1\r\n1\r\n$-1\r\n",
		string(rawReplyOn(t, e, "HMGET", "h", "a", "absent")))
	assert.Equal(t, "*2\r\n$-1\r\n$-1\r\n",
		string(rawReplyOn(t, e, "HMGET", "absent", "a", "b")),
		"HMGET on a missing key is nils, not an error")
}

func TestHGetAllPairsFieldsWithValues(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "HSET", "h", "a", "1", "b", "2", "c", "3")

	flat := toStrings(runOn(t, e, "HGETALL", "h"))
	assert.Len(t, flat, 6, "flat field,value,field,value - what a RESP2 client decodes into a map")

	got := map[string]string{}
	for i := 0; i < len(flat); i += 2 {
		got[flat[i]] = flat[i+1]
	}
	assert.Equal(t, map[string]string{"a": "1", "b": "2", "c": "3"}, got)

	assert.Empty(t, toStrings(runOn(t, e, "HGETALL", "absent")))
}

func TestHKeysAndHValsAgreeWithHGetAll(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "HSET", "h", "a", "1", "b", "2")
	assert.ElementsMatch(t, []string{"a", "b"}, toStrings(runOn(t, e, "HKEYS", "h")))
	assert.ElementsMatch(t, []string{"1", "2"}, toStrings(runOn(t, e, "HVALS", "h")))
}

func TestHSetNXOnlySetsWhatIsNotThere(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.Equal(t, int64(1), runOn(t, e, "HSETNX", "h", "a", "first"))
	assert.Equal(t, int64(0), runOn(t, e, "HSETNX", "h", "a", "second"))
	assert.Equal(t, "first", runOn(t, e, "HGET", "h", "a"), "and leaves the value alone")
}

func TestHIncrByTreatsMissingAsZero(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.Equal(t, int64(5), runOn(t, e, "HINCRBY", "h", "n", "5"), "a missing field starts at zero")
	assert.Equal(t, int64(8), runOn(t, e, "HINCRBY", "h", "n", "3"))
	assert.Equal(t, int64(-2), runOn(t, e, "HINCRBY", "h", "n", "-10"))
	assert.Equal(t, "-2", runOn(t, e, "HGET", "h", "n"))
}

func TestHIncrByRefusesANonIntegerFieldAndLeavesIt(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "HSET", "h", "word", "hello")
	res, _ := runOn(t, e, "HINCRBY", "h", "word", "1").(string)
	assert.Contains(t, res, "not an integer")
	assert.Equal(t, "hello", runOn(t, e, "HGET", "h", "word"),
		"a refused increment must not reset the field to the increment")
}

// TestHIncrByOnAnInvalidFieldCreatesNoEmptyHash is why the hash is created
// only after the increment is known to be valid. An empty hash is a key that
// answers EXISTS 1 and HGETALL nothing, and that a rewrite writes as an HSET
// with no pairs - a syntax error on replay.
func TestHIncrByOnAnInvalidFieldCreatesNoEmptyHash(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	res, _ := runOn(t, e, "HINCRBY", "absent", "n", "not-a-number").(string)
	assert.Contains(t, res, "not an integer")
	assert.Equal(t, int64(0), runOn(t, e, "EXISTS", "absent"),
		"a failed increment leaves no key behind")
}

// TestRemovingTheLastFieldRemovesTheKey: Redis has no empty hash, and neither
// can this - see the comment on dropIfEmpty.
func TestRemovingTheLastFieldRemovesTheKey(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "HSET", "h", "a", "1", "b", "2")
	assert.Equal(t, int64(1), runOn(t, e, "EXISTS", "h"))

	assert.Equal(t, int64(1), runOn(t, e, "HDEL", "h", "a"))
	assert.Equal(t, int64(1), runOn(t, e, "EXISTS", "h"), "one field left, so the key remains")

	assert.Equal(t, int64(1), runOn(t, e, "HDEL", "h", "b"))
	assert.Equal(t, int64(0), runOn(t, e, "EXISTS", "h"),
		"the last field going takes the key with it")
	assert.Equal(t, "none", runOn(t, e, "TYPE", "h"))
	assert.Equal(t, int64(0), runOn(t, e, "HLEN", "h"))
}

func TestHashIsItsOwnTypeAcrossTheKeyspace(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "HSET", "h", "a", "1")

	assert.Equal(t, "hash", runOn(t, e, "TYPE", "h"), "the word Redis uses")
	assert.Equal(t, int64(1), runOn(t, e, "EXISTS", "h"))
	assert.Equal(t, int64(1), runOn(t, e, "DBSIZE"))
	assert.Equal(t, []string{"h"}, toStrings(runOn(t, e, "KEYS", "*")))

	res, _ := runOn(t, e, "SADD", "h", "member").(string)
	assert.Contains(t, res, "WRONGTYPE", "a name held by a hash is refused to every other type")
	res, _ = runOn(t, e, "GET", "h").(string)
	assert.Contains(t, res, "WRONGTYPE")

	assert.Equal(t, int64(1), runOn(t, e, "DEL", "h"), "and DEL removes whichever type holds it")
	assert.Equal(t, int64(0), runOn(t, e, "EXISTS", "h"))
}

func TestHashCommandsRefuseAKeyOfAnotherType(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "SET", "str", "v")
	for _, cmd := range [][]string{
		{"HSET", "str", "a", "1"}, {"HGET", "str", "a"}, {"HDEL", "str", "a"},
		{"HLEN", "str"}, {"HGETALL", "str"}, {"HINCRBY", "str", "a", "1"},
	} {
		res, _ := runOn(t, e, cmd[0], cmd[1:]...).(string)
		assert.Contains(t, res, "WRONGTYPE", "%s against a string", cmd[0])
	}
}

func TestHashCountsTowardTheMemoryBudget(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	before := e.space.TotalMemUsed()
	runOn(t, e, "HSET", "h", "field", "a value of some length")
	assert.Greater(t, e.space.TotalMemUsed(), before,
		"a hash is accounted, or a keyspace full of them sails past -maxmemory")

	runOn(t, e, "DEL", "h")
	assert.Equal(t, before, e.space.TotalMemUsed(),
		"and gives back exactly what it took")
}

func TestHashSurvivesARestart(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := withAOFOn(t, e, func() {
		runOn(t, e, "HSET", "h", "a", "1", "b", "2")
		runOn(t, e, "HINCRBY", "h", "n", "41")
		runOn(t, e, "HINCRBY", "h", "n", "1")
		runOn(t, e, "HSETNX", "h", "c", "3")
		runOn(t, e, "HDEL", "h", "a")
	})
	restartOn(t, e, path)

	assert.Equal(t, "hash", runOn(t, e, "TYPE", "h"))
	assert.Equal(t, int64(3), runOn(t, e, "HLEN", "h"))
	assert.Equal(t, "42", runOn(t, e, "HGET", "h", "n"), "increments replay to the same number")
	assert.Equal(t, "2", runOn(t, e, "HGET", "h", "b"))
	assert.Equal(t, "3", runOn(t, e, "HGET", "h", "c"))
	assert.Equal(t, "$-1\r\n", string(rawReplyOn(t, e, "HGET", "h", "a")),
		"and a deleted field stays deleted")
}

// TestARewrittenLogRebuildsTheHash: a hash has a command that rebuilds it, so
// the rewrite writes HSET rather than falling through to KEEL.RESTORE.
func TestARewrittenLogRebuildsTheHash(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := withAOFOn(t, e, func() {
		for i := 0; i < 200; i++ {
			runOn(t, e, "HINCRBY", "h", "counter", "1")
		}
		runOn(t, e, "HSET", "h", "name", "value")
		assert.NoError(t, e.RewriteAOF())
	})
	restartOn(t, e, path)

	assert.Equal(t, "200", runOn(t, e, "HGET", "h", "counter"),
		"200 increments collapse to one HSET holding the answer")
	assert.Equal(t, "value", runOn(t, e, "HGET", "h", "name"))
	assert.Equal(t, int64(2), runOn(t, e, "HLEN", "h"))
}

// TestARewriteCarriesEveryKeyspaceForward is the regression for the bug adding
// hashes exposed.
//
// The rewrite collected key names from a hand-written list of the stores, so
// the first type added after that list was written was absent from it. The walk
// then found nothing to write for those keys and the rewritten log dropped
// every hash in the keyspace - silently, since a rewrite that writes less is
// exactly what a rewrite is supposed to do. The list is now the keyspace
// registry, so it cannot be one type short.
func TestARewriteCarriesEveryKeyspaceForward(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := withAOFOn(t, e, func() {
		runOn(t, e, "SET", "a-string", "v")
		runOn(t, e, "HSET", "a-hash", "f", "v")
		runOn(t, e, "SADD", "a-set", "m")
		runOn(t, e, "ZADD", "a-zset", "1", "m")
		runOn(t, e, "PFADD", "a-hll", "x")
		runOn(t, e, "CF.ADD", "a-cuckoo", "x")
		runOn(t, e, "BF.MADD", "a-bloom", "x")
		runOn(t, e, "CMS.INITBYDIM", "a-cms", "100", "5")
		runOn(t, e, "MORRIS.INITBYDIM", "a-morris", "100", "5")
		before := runOn(t, e, "DBSIZE")
		assert.NoError(t, e.RewriteAOF())
		assert.Equal(t, before, runOn(t, e, "DBSIZE"), "a rewrite changes no keys")
	})

	restartOn(t, e, path)
	assert.Equal(t, int64(9), runOn(t, e, "DBSIZE"),
		"every keyspace has to survive a rewrite, not just the ones a list remembered")
	for _, k := range []string{"a-string", "a-hash", "a-set", "a-zset", "a-hll",
		"a-cuckoo", "a-bloom", "a-cms", "a-morris"} {
		assert.Equal(t, int64(1), runOn(t, e, "EXISTS", k), "%s survived the rewrite", k)
	}
}
