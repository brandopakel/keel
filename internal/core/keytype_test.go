package core

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/brandopakel/keel/internal/data_structure"
)

// TestOneNameHoldsOneType is the bug this exists for.
//
// Each type keeps its own map, so a name used to be unique only within a type:
// SET k v and SADD k m both succeeded, GET and SMEMBERS both answered, and DEL
// removed the string and left the set. A client reusing a name across types was
// never told, and could not delete what it had made.
func TestOneNameHoldsOneType(t *testing.T) {
	ResetStores()
	assert.EqualValues(t, "OK", run(t, "SET", "dual", "i-am-a-string"))

	res := run(t, "SADD", "dual", "member")
	assert.Contains(t, res, "WRONGTYPE", "a name already holding a string must refuse a set")
	assert.EqualValues(t, "i-am-a-string", run(t, "GET", "dual"), "and must not have been disturbed")

	// Once it is gone the name is free again, for any type.
	assert.EqualValues(t, 1, run(t, "DEL", "dual"))
	assert.EqualValues(t, 1, run(t, "SADD", "dual", "member"))
	assert.EqualValues(t, 1, run(t, "SCARD", "dual"))
}

// TestWrongTypeCoversEveryKeyspace. The table is written by hand, so what
// matters is that no keyspace was left out of it.
func TestWrongTypeCoversEveryKeyspace(t *testing.T) {
	makers := []struct {
		name string
		make func()
		read []string
	}{
		{"string", func() { run(t, "SET", "k", "v") }, []string{"GET", "k"}},
		{"set", func() { run(t, "SADD", "k", "m") }, []string{"SCARD", "k"}},
		{"zset", func() { run(t, "ZADD", "k", "1", "m") }, []string{"ZCARD", "k"}},
		{"bloom", func() { run(t, "BF.MADD", "k", "m") }, []string{"BF.EXISTS", "k", "m"}},
		{"cms", func() { run(t, "CMS.INITBYDIM", "k", "100", "5") }, []string{"CMS.QUERY", "k", "m"}},
		{"morris", func() { run(t, "MORRIS.INITBYDIM", "k", "100", "5") }, []string{"MORRIS.QUERY", "k", "m"}},
		{"hll", func() { run(t, "PFADD", "k", "m") }, []string{"PFCOUNT", "k"}},
		{"cuckoo", func() { run(t, "CF.ADD", "k", "m") }, []string{"CF.EXISTS", "k", "m"}},
	}

	for _, holder := range makers {
		for _, intruder := range makers {
			if holder.name == intruder.name {
				continue
			}
			ResetStores()
			holder.make()
			assert.Equal(t, 1, data_structure.TotalKeys(), "%s should hold the name", holder.name)

			res := run(t, intruder.read[0], intruder.read[1:]...)
			assert.Contains(t, res, "WRONGTYPE",
				"%s reading a name held by %s must be refused", intruder.name, holder.name)
			assert.Equal(t, 1, data_structure.TotalKeys(),
				"%s must not have created a second key called k", intruder.name)
		}
	}
}

// TestDelRemovesWhicheverTypeHoldsTheName. DEL used to look only in the string
// dictionary, so it reported nothing deleted for every other type - a delete
// that answers and does nothing.
func TestDelRemovesWhicheverTypeHoldsTheName(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func()
	}{
		{"set", func() { run(t, "SADD", "k", "m") }},
		{"zset", func() { run(t, "ZADD", "k", "1", "m") }},
		{"hll", func() { run(t, "PFADD", "k", "m") }},
		{"cuckoo", func() { run(t, "CF.ADD", "k", "m") }},
		{"cms", func() { run(t, "CMS.INITBYDIM", "k", "100", "5") }},
		{"morris", func() { run(t, "MORRIS.INITBYDIM", "k", "100", "5") }},
		{"bloom", func() { run(t, "BF.MADD", "k", "m") }},
		{"string", func() { run(t, "SET", "k", "v") }},
	} {
		ResetStores()
		tc.make()
		assert.Equal(t, 1, data_structure.TotalKeys(), tc.name)

		assert.EqualValues(t, 1, run(t, "DEL", "k"), "DEL must report deleting a %s", tc.name)
		assert.Equal(t, 0, data_structure.TotalKeys(), "and must actually remove the %s", tc.name)
	}
}

func TestDelCountsOnlyWhatItRemoved(t *testing.T) {
	ResetStores()
	run(t, "SET", "a", "v")
	run(t, "SADD", "b", "m")
	assert.EqualValues(t, 2, run(t, "DEL", "a", "b", "never-existed"))
	assert.Equal(t, 0, data_structure.TotalKeys())
}

// TestGeoSharesTheSortedSetKeyspace, as it does in Redis: the geohash is the
// score. Adding a member to a geo key with ZADD is therefore legal, and must
// not be refused by a table that guessed geo had a keyspace of its own.
func TestGeoSharesTheSortedSetKeyspace(t *testing.T) {
	ResetStores()
	assert.EqualValues(t, 1, run(t, "GEOADD", "places", "13.361389", "38.115556", "palermo"))

	assert.EqualValues(t, 1, run(t, "ZCARD", "places"), "a geo key is a sorted set")
	res := run(t, "SADD", "places", "m")
	assert.Contains(t, res, "WRONGTYPE", "but still not a set")
}

// TestMultiKeyCommandsCheckEveryKey. PFCOUNT and PFMERGE name several keys, and
// checking only the first would let the rest through unexamined.
func TestMultiKeyCommandsCheckEveryKey(t *testing.T) {
	ResetStores()
	run(t, "PFADD", "hll", "a")
	run(t, "SET", "str", "v")

	assert.Contains(t, run(t, "PFCOUNT", "hll", "str"), "WRONGTYPE",
		"a string in the second position must be caught")
	assert.Contains(t, run(t, "PFMERGE", "dest", "hll", "str"), "WRONGTYPE")
}

// TestTypeCheckLetsAFreeNameThrough. The check must only refuse names that are
// actually taken, or every first write would fail.
func TestTypeCheckLetsAFreeNameThrough(t *testing.T) {
	ResetStores()
	assert.EqualValues(t, "OK", run(t, "SET", "fresh", "v"))
	assert.EqualValues(t, 1, run(t, "SADD", "other", "m"))
	assert.EqualValues(t, 2, data_structure.TotalKeys())
}

// TestExpiredKeyDoesNotHoldItsName. A string whose TTL has passed owns nothing,
// so another type must be able to take the name.
func TestExpiredKeyDoesNotHoldItsName(t *testing.T) {
	ResetStores()
	assert.EqualValues(t, "OK", run(t, "SET", "temp", "v", "PX", "1"))

	deadline := timeAfter(50)
	for !deadline() {
	}

	assert.EqualValues(t, 1, run(t, "SADD", "temp", "m"),
		"a name whose string has expired must be free for another type")
	assert.EqualValues(t, 1, run(t, "SCARD", "temp"))
}

// timeAfter returns a predicate that becomes true once ms milliseconds have
// passed, for the tests that have to outlast a TTL.
func timeAfter(ms int) func() bool {
	deadline := time.Now().Add(time.Duration(ms) * time.Millisecond)
	return func() bool { return time.Now().After(deadline) }
}
