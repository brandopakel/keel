package core

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/brandopakel/keel/internal/data_structure"
)

func TestHSetCountsNewFieldsOnly(t *testing.T) {
	ResetStores()
	assert.Equal(t, int64(2), run(t, "HSET", "h", "a", "1", "b", "2"))
	assert.Equal(t, int64(0), run(t, "HSET", "h", "a", "changed"),
		"an overwrite is not a new field, even when the value changes")
	assert.Equal(t, int64(1), run(t, "HSET", "h", "c", "3"))
	assert.Equal(t, "changed", run(t, "HGET", "h", "a"))
	assert.Equal(t, int64(3), run(t, "HLEN", "h"))
}

func TestHSetRefusesAnIncompletePair(t *testing.T) {
	ResetStores()
	res, _ := run(t, "HSET", "h", "a", "1", "b").(string)
	assert.Contains(t, res, "wrong number of arguments")
	assert.Equal(t, int64(0), run(t, "EXISTS", "h"), "and creates nothing")
}

func TestHGetAndHMGetOnMissingThings(t *testing.T) {
	ResetStores()
	run(t, "HSET", "h", "a", "1")

	assert.Equal(t, "$-1\r\n", string(rawReply(t, "HGET", "h", "absent")),
		"a missing field is a null, not an empty string")
	assert.Equal(t, "$-1\r\n", string(rawReply(t, "HGET", "absent", "a")),
		"and so is a missing key")
	assert.Equal(t, "*2\r\n$1\r\n1\r\n$-1\r\n",
		string(rawReply(t, "HMGET", "h", "a", "absent")))
	assert.Equal(t, "*2\r\n$-1\r\n$-1\r\n",
		string(rawReply(t, "HMGET", "absent", "a", "b")),
		"HMGET on a missing key is nils, not an error")
}

func TestHGetAllPairsFieldsWithValues(t *testing.T) {
	ResetStores()
	run(t, "HSET", "h", "a", "1", "b", "2", "c", "3")

	flat := toStrings(run(t, "HGETALL", "h"))
	assert.Len(t, flat, 6, "flat field,value,field,value - what a RESP2 client decodes into a map")

	got := map[string]string{}
	for i := 0; i < len(flat); i += 2 {
		got[flat[i]] = flat[i+1]
	}
	assert.Equal(t, map[string]string{"a": "1", "b": "2", "c": "3"}, got)

	assert.Empty(t, toStrings(run(t, "HGETALL", "absent")))
}

func TestHKeysAndHValsAgreeWithHGetAll(t *testing.T) {
	ResetStores()
	run(t, "HSET", "h", "a", "1", "b", "2")
	assert.ElementsMatch(t, []string{"a", "b"}, toStrings(run(t, "HKEYS", "h")))
	assert.ElementsMatch(t, []string{"1", "2"}, toStrings(run(t, "HVALS", "h")))
}

func TestHSetNXOnlySetsWhatIsNotThere(t *testing.T) {
	ResetStores()
	assert.Equal(t, int64(1), run(t, "HSETNX", "h", "a", "first"))
	assert.Equal(t, int64(0), run(t, "HSETNX", "h", "a", "second"))
	assert.Equal(t, "first", run(t, "HGET", "h", "a"), "and leaves the value alone")
}

func TestHIncrByTreatsMissingAsZero(t *testing.T) {
	ResetStores()
	assert.Equal(t, int64(5), run(t, "HINCRBY", "h", "n", "5"), "a missing field starts at zero")
	assert.Equal(t, int64(8), run(t, "HINCRBY", "h", "n", "3"))
	assert.Equal(t, int64(-2), run(t, "HINCRBY", "h", "n", "-10"))
	assert.Equal(t, "-2", run(t, "HGET", "h", "n"))
}

func TestHIncrByRefusesANonIntegerFieldAndLeavesIt(t *testing.T) {
	ResetStores()
	run(t, "HSET", "h", "word", "hello")
	res, _ := run(t, "HINCRBY", "h", "word", "1").(string)
	assert.Contains(t, res, "not an integer")
	assert.Equal(t, "hello", run(t, "HGET", "h", "word"),
		"a refused increment must not reset the field to the increment")
}

// TestHIncrByOnAnInvalidFieldCreatesNoEmptyHash is why the hash is created
// only after the increment is known to be valid. An empty hash is a key that
// answers EXISTS 1 and HGETALL nothing, and that a rewrite writes as an HSET
// with no pairs - a syntax error on replay.
func TestHIncrByOnAnInvalidFieldCreatesNoEmptyHash(t *testing.T) {
	ResetStores()
	res, _ := run(t, "HINCRBY", "absent", "n", "not-a-number").(string)
	assert.Contains(t, res, "not an integer")
	assert.Equal(t, int64(0), run(t, "EXISTS", "absent"),
		"a failed increment leaves no key behind")
}

// TestRemovingTheLastFieldRemovesTheKey: Redis has no empty hash, and neither
// can this - see the comment on dropIfEmpty.
func TestRemovingTheLastFieldRemovesTheKey(t *testing.T) {
	ResetStores()
	run(t, "HSET", "h", "a", "1", "b", "2")
	assert.Equal(t, int64(1), run(t, "EXISTS", "h"))

	assert.Equal(t, int64(1), run(t, "HDEL", "h", "a"))
	assert.Equal(t, int64(1), run(t, "EXISTS", "h"), "one field left, so the key remains")

	assert.Equal(t, int64(1), run(t, "HDEL", "h", "b"))
	assert.Equal(t, int64(0), run(t, "EXISTS", "h"),
		"the last field going takes the key with it")
	assert.Equal(t, "none", run(t, "TYPE", "h"))
	assert.Equal(t, int64(0), run(t, "HLEN", "h"))
}

func TestHashIsItsOwnTypeAcrossTheKeyspace(t *testing.T) {
	ResetStores()
	run(t, "HSET", "h", "a", "1")

	assert.Equal(t, "hash", run(t, "TYPE", "h"), "the word Redis uses")
	assert.Equal(t, int64(1), run(t, "EXISTS", "h"))
	assert.Equal(t, int64(1), run(t, "DBSIZE"))
	assert.Equal(t, []string{"h"}, toStrings(run(t, "KEYS", "*")))

	res, _ := run(t, "SADD", "h", "member").(string)
	assert.Contains(t, res, "WRONGTYPE", "a name held by a hash is refused to every other type")
	res, _ = run(t, "GET", "h").(string)
	assert.Contains(t, res, "WRONGTYPE")

	assert.Equal(t, int64(1), run(t, "DEL", "h"), "and DEL removes whichever type holds it")
	assert.Equal(t, int64(0), run(t, "EXISTS", "h"))
}

func TestHashCommandsRefuseAKeyOfAnotherType(t *testing.T) {
	ResetStores()
	run(t, "SET", "str", "v")
	for _, cmd := range [][]string{
		{"HSET", "str", "a", "1"}, {"HGET", "str", "a"}, {"HDEL", "str", "a"},
		{"HLEN", "str"}, {"HGETALL", "str"}, {"HINCRBY", "str", "a", "1"},
	} {
		res, _ := run(t, cmd[0], cmd[1:]...).(string)
		assert.Contains(t, res, "WRONGTYPE", "%s against a string", cmd[0])
	}
}

func TestHashCountsTowardTheMemoryBudget(t *testing.T) {
	ResetStores()
	before := data_structure.TotalMemUsed()
	run(t, "HSET", "h", "field", "a value of some length")
	assert.Greater(t, data_structure.TotalMemUsed(), before,
		"a hash is accounted, or a keyspace full of them sails past -maxmemory")

	run(t, "DEL", "h")
	assert.Equal(t, before, data_structure.TotalMemUsed(),
		"and gives back exactly what it took")
}

func TestHashSurvivesARestart(t *testing.T) {
	path := withAOF(t, func() {
		run(t, "HSET", "h", "a", "1", "b", "2")
		run(t, "HINCRBY", "h", "n", "41")
		run(t, "HINCRBY", "h", "n", "1")
		run(t, "HSETNX", "h", "c", "3")
		run(t, "HDEL", "h", "a")
	})
	restart(t, path)

	assert.Equal(t, "hash", run(t, "TYPE", "h"))
	assert.Equal(t, int64(3), run(t, "HLEN", "h"))
	assert.Equal(t, "42", run(t, "HGET", "h", "n"), "increments replay to the same number")
	assert.Equal(t, "2", run(t, "HGET", "h", "b"))
	assert.Equal(t, "3", run(t, "HGET", "h", "c"))
	assert.Equal(t, "$-1\r\n", string(rawReply(t, "HGET", "h", "a")),
		"and a deleted field stays deleted")
}

// TestARewrittenLogRebuildsTheHash: a hash has a command that rebuilds it, so
// the rewrite writes HSET rather than falling through to KEEL.RESTORE.
func TestARewrittenLogRebuildsTheHash(t *testing.T) {
	path := withAOF(t, func() {
		for i := 0; i < 200; i++ {
			run(t, "HINCRBY", "h", "counter", "1")
		}
		run(t, "HSET", "h", "name", "value")
		assert.NoError(t, RewriteAOF())
	})
	restart(t, path)

	assert.Equal(t, "200", run(t, "HGET", "h", "counter"),
		"200 increments collapse to one HSET holding the answer")
	assert.Equal(t, "value", run(t, "HGET", "h", "name"))
	assert.Equal(t, int64(2), run(t, "HLEN", "h"))
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
	path := withAOF(t, func() {
		run(t, "SET", "a-string", "v")
		run(t, "HSET", "a-hash", "f", "v")
		run(t, "SADD", "a-set", "m")
		run(t, "ZADD", "a-zset", "1", "m")
		run(t, "PFADD", "a-hll", "x")
		run(t, "CF.ADD", "a-cuckoo", "x")
		run(t, "BF.MADD", "a-bloom", "x")
		run(t, "CMS.INITBYDIM", "a-cms", "100", "5")
		run(t, "MORRIS.INITBYDIM", "a-morris", "100", "5")
		before := run(t, "DBSIZE")
		assert.NoError(t, RewriteAOF())
		assert.Equal(t, before, run(t, "DBSIZE"), "a rewrite changes no keys")
	})

	restart(t, path)
	assert.Equal(t, int64(9), run(t, "DBSIZE"),
		"every keyspace has to survive a rewrite, not just the ones a list remembered")
	for _, k := range []string{"a-string", "a-hash", "a-set", "a-zset", "a-hll",
		"a-cuckoo", "a-bloom", "a-cms", "a-morris"} {
		assert.Equal(t, int64(1), run(t, "EXISTS", k), "%s survived the rewrite", k)
	}
}
