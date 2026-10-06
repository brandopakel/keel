package core

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestOneNameHoldsOneType is the bug this exists for.
//
// Each type keeps its own map, so a name used to be unique only within a type:
// SET k v and SADD k m both succeeded, GET and SMEMBERS both answered, and DEL
// removed the string and left the set. A client reusing a name across types was
// never told, and could not delete what it had made.
func TestOneNameHoldsOneType(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.EqualValues(t, "OK", runOn(t, e, "SET", "dual", "i-am-a-string"))

	res := runOn(t, e, "SADD", "dual", "member")
	assert.Contains(t, res, "WRONGTYPE", "a name already holding a string must refuse a set")
	assert.EqualValues(t, "i-am-a-string", runOn(t, e, "GET", "dual"), "and must not have been disturbed")

	// Once it is gone the name is free again, for any type.
	assert.EqualValues(t, 1, runOn(t, e, "DEL", "dual"))
	assert.EqualValues(t, 1, runOn(t, e, "SADD", "dual", "member"))
	assert.EqualValues(t, 1, runOn(t, e, "SCARD", "dual"))
}

// TestWrongTypeCoversEveryKeyspace. The table is written by hand, so what
// matters is that no keyspace was left out of it.
func TestWrongTypeCoversEveryKeyspace(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	makers := []struct {
		name string
		make func()
		read []string
	}{
		{"string", func() { runOn(t, e, "SET", "k", "v") }, []string{"GET", "k"}},
		{"set", func() { runOn(t, e, "SADD", "k", "m") }, []string{"SCARD", "k"}},
		{"zset", func() { runOn(t, e, "ZADD", "k", "1", "m") }, []string{"ZCARD", "k"}},
		// BF.EXISTS and CF.EXISTS answer no for another type, as RedisBloom's
		// do - see TestBFReadsAnswerNoForAnotherType - so the filters are read
		// with their INFO commands, which answer WRONGTYPE.
		{"bloom", func() { runOn(t, e, "BF.MADD", "k", "m") }, []string{"BF.INFO", "k"}},
		{"cms", func() { runOn(t, e, "CMS.INITBYDIM", "k", "100", "5") }, []string{"CMS.QUERY", "k", "m"}},
		{"morris", func() { runOn(t, e, "MORRIS.INITBYDIM", "k", "100", "5") }, []string{"MORRIS.QUERY", "k", "m"}},
		{"hll", func() { runOn(t, e, "PFADD", "k", "m") }, []string{"PFCOUNT", "k"}},
		{"cuckoo", func() { runOn(t, e, "CF.ADD", "k", "m") }, []string{"CF.INFO", "k"}},
	}

	for _, holder := range makers {
		for _, intruder := range makers {
			if holder.name == intruder.name {
				continue
			}
			e.resetStores()
			holder.make()
			assert.Equal(t, 1, e.space.TotalKeys(), "%s should hold the name", holder.name)

			res := runOn(t, e, intruder.read[0], intruder.read[1:]...)
			assert.Contains(t, res, "WRONGTYPE",
				"%s reading a name held by %s must be refused", intruder.name, holder.name)
			assert.Equal(t, 1, e.space.TotalKeys(),
				"%s must not have created a second key called k", intruder.name)
		}
	}
}

// TestDelRemovesWhicheverTypeHoldsTheName. DEL used to look only in the string
// dictionary, so it reported nothing deleted for every other type - a delete
// that answers and does nothing.
func TestDelRemovesWhicheverTypeHoldsTheName(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	for _, tc := range []struct {
		name string
		make func()
	}{
		{"set", func() { runOn(t, e, "SADD", "k", "m") }},
		{"zset", func() { runOn(t, e, "ZADD", "k", "1", "m") }},
		{"hll", func() { runOn(t, e, "PFADD", "k", "m") }},
		{"cuckoo", func() { runOn(t, e, "CF.ADD", "k", "m") }},
		{"cms", func() { runOn(t, e, "CMS.INITBYDIM", "k", "100", "5") }},
		{"morris", func() { runOn(t, e, "MORRIS.INITBYDIM", "k", "100", "5") }},
		{"bloom", func() { runOn(t, e, "BF.MADD", "k", "m") }},
		{"string", func() { runOn(t, e, "SET", "k", "v") }},
	} {
		e.resetStores()
		tc.make()
		assert.Equal(t, 1, e.space.TotalKeys(), tc.name)

		assert.EqualValues(t, 1, runOn(t, e, "DEL", "k"), "DEL must report deleting a %s", tc.name)
		assert.Equal(t, 0, e.space.TotalKeys(), "and must actually remove the %s", tc.name)
	}
}

func TestDelCountsOnlyWhatItRemoved(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "SET", "a", "v")
	runOn(t, e, "SADD", "b", "m")
	assert.EqualValues(t, 2, runOn(t, e, "DEL", "a", "b", "never-existed"))
	assert.Equal(t, 0, e.space.TotalKeys())
}

// TestGeoSharesTheSortedSetKeyspace, as it does in Redis: the geohash is the
// score. Adding a member to a geo key with ZADD is therefore legal, and must
// not be refused by a table that guessed geo had a keyspace of its own.
func TestGeoSharesTheSortedSetKeyspace(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.EqualValues(t, 1, runOn(t, e, "GEOADD", "places", "13.361389", "38.115556", "palermo"))

	assert.EqualValues(t, 1, runOn(t, e, "ZCARD", "places"), "a geo key is a sorted set")
	res := runOn(t, e, "SADD", "places", "m")
	assert.Contains(t, res, "WRONGTYPE", "but still not a set")
}

// TestMultiKeyCommandsCheckEveryKey. PFCOUNT and PFMERGE name several keys, and
// checking only the first would let the rest through unexamined.
func TestMultiKeyCommandsCheckEveryKey(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "PFADD", "hll", "a")
	runOn(t, e, "SET", "str", "v")

	assert.Contains(t, runOn(t, e, "PFCOUNT", "hll", "str"), "WRONGTYPE",
		"a string in the second position must be caught")
	assert.Contains(t, runOn(t, e, "PFMERGE", "dest", "hll", "str"), "WRONGTYPE")
}

// TestTypeCheckLetsAFreeNameThrough. The check must only refuse names that are
// actually taken, or every first write would fail.
func TestTypeCheckLetsAFreeNameThrough(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.EqualValues(t, "OK", runOn(t, e, "SET", "fresh", "v"))
	assert.EqualValues(t, 1, runOn(t, e, "SADD", "other", "m"))
	assert.EqualValues(t, 2, e.space.TotalKeys())
}

// TestExpiredKeyDoesNotHoldItsName. A string whose TTL has passed owns nothing,
// so another type must be able to take the name.
func TestExpiredKeyDoesNotHoldItsName(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.EqualValues(t, "OK", runOn(t, e, "SET", "temp", "v", "PX", "1"))

	deadline := timeAfter(50)
	for !deadline() {
	}

	assert.EqualValues(t, 1, runOn(t, e, "SADD", "temp", "m"),
		"a name whose string has expired must be free for another type")
	assert.EqualValues(t, 1, runOn(t, e, "SCARD", "temp"))
}

// timeAfter returns a predicate that becomes true once ms milliseconds have
// passed, for the tests that have to outlast a TTL.
func timeAfter(ms int) func() bool {
	deadline := time.Now().Add(time.Duration(ms) * time.Millisecond)
	return func() bool { return time.Now().After(deadline) }
}

// TestTypeRuleIsIndexedWithTheCommand: dispatch reads the type check's rule
// from the command's index entry, so the entry has to say what the tables say,
// for every command, and find the same keys commandKeys does.
func TestTypeRuleIsIndexedWithTheCommand(t *testing.T) {
	t.Parallel()
	for name, entry := range commands {
		space, checked := commandKeyspace[name]
		if !checked || replacingWrites[name] || filterCommands[name] {
			space = ""
		}
		assert.Equal(t, space, entry.typed, name)
		for n := 0; n <= 5; n++ {
			args := make([]string, n)
			for i := range args {
				args[i] = string(rune('a' + i))
			}
			cmd := &Command{Cmd: name, Args: args}
			assert.Equal(t, commandKeys(cmd), keysBy(cmd, entry.keys), "%s with %d arguments", name, n)
		}
	}
	for name, want := range map[string]keyRule{
		"GET": keyFirst, "HSET": keyFirst, "LCS": keyFirstTwo, "MSET": keyStride,
		"PFCOUNT": keyEvery, "PFMERGE": keyEvery,
	} {
		assert.Equal(t, want, commands[name].keys, name)
	}
	// commandKeys finds keys through keysBy too, so the comparison above cannot
	// catch a rule that picks the wrong arguments on both sides. These keys are
	// written out by hand.
	for _, c := range []struct {
		name string
		args []string
		want []string
	}{
		{"GET", []string{"a"}, []string{"a"}},
		{"HSET", []string{"a", "f", "v"}, []string{"a"}},
		{"LCS", []string{"a", "b", "LEN"}, []string{"a", "b"}},
		{"LCS", []string{"a"}, []string{"a"}},
		{"MSET", []string{"a", "1", "b", "2", "c", "3"}, []string{"a", "b", "c"}},
		{"MSET", []string{"a", "1"}, []string{"a"}},
		{"PFCOUNT", []string{"a", "b", "c"}, []string{"a", "b", "c"}},
		{"PFMERGE", []string{"dest", "a", "b"}, []string{"dest", "a", "b"}},
		{"GET", nil, nil},
	} {
		cmd := &Command{Cmd: c.name, Args: c.args}
		assert.Equal(t, c.want, keysBy(cmd, commands[c.name].keys), "%s %v", c.name, c.args)
		assert.Equal(t, c.want, commandKeys(cmd), "%s %v", c.name, c.args)
	}
	for name, want := range map[string]string{
		"GET": "string", "HSET": "hash", "ZADD": "zset", "PFADD": "hll",
		"SET": "", "MSET": "", "BF.ADD": "", "CF.EXISTS": "", "DEL": "", "TYPE": "",
	} {
		assert.Equal(t, want, commands[name].typed, name)
	}
}
