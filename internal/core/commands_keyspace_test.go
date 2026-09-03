package core

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"

	"memkv/internal/data_structure"
)

// toStrings flattens the array reply KEYS and friends answer with.
func toStrings(reply interface{}) []string {
	items, ok := reply.([]interface{})
	if !ok {
		if ss, ok := reply.([]string); ok {
			return ss
		}
		return nil
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		s, _ := it.(string)
		out = append(out, s)
	}
	return out
}

func TestExistsCountsAcrossEveryKeyspace(t *testing.T) {
	ResetStores()
	run(t, "SET", "s", "v")
	run(t, "SADD", "theset", "m")
	run(t, "ZADD", "thezset", "1", "m")
	run(t, "PFADD", "thehll", "m")

	assert.Equal(t, int64(1), run(t, "EXISTS", "s"))
	assert.Equal(t, int64(1), run(t, "EXISTS", "theset"),
		"a set is a key, and EXISTS is not a string command")
	assert.Equal(t, int64(1), run(t, "EXISTS", "thezset"))
	assert.Equal(t, int64(1), run(t, "EXISTS", "thehll"))
	assert.Equal(t, int64(0), run(t, "EXISTS", "absent"))

	assert.Equal(t, int64(4), run(t, "EXISTS", "s", "theset", "thezset", "thehll"))
	assert.Equal(t, int64(2), run(t, "EXISTS", "s", "absent", "theset"))
}

// TestExistsCountsRepeatsRepeatedly is Redis's behaviour from 3.0 onwards, and
// the one people find surprising often enough to be worth pinning.
func TestExistsCountsRepeatsRepeatedly(t *testing.T) {
	ResetStores()
	run(t, "SET", "k", "v")
	assert.Equal(t, int64(3), run(t, "EXISTS", "k", "k", "k"))
	assert.Equal(t, int64(0), run(t, "EXISTS", "nope", "nope"))
}

func TestExistsDoesNotSeeAnExpiredKey(t *testing.T) {
	ResetStores()
	run(t, "SET", "k", "v", "PX", "1")
	assert.Equal(t, int64(1), run(t, "EXISTS", "k"))
	waitPast(5)
	assert.Equal(t, int64(0), run(t, "EXISTS", "k"),
		"a key past its TTL exists no more than a deleted one")
}

func TestTypeNamesTheKeyspaceHoldingTheKey(t *testing.T) {
	ResetStores()
	run(t, "SET", "s", "v")
	run(t, "SADD", "theset", "m")
	run(t, "ZADD", "thezset", "1", "m")
	run(t, "PFADD", "thehll", "m")
	run(t, "CF.ADD", "thecf", "m")

	// The first three are the words Redis uses, so a client switching on the
	// reply behaves the same against either server.
	assert.Equal(t, "string", run(t, "TYPE", "s"))
	assert.Equal(t, "set", run(t, "TYPE", "theset"))
	assert.Equal(t, "zset", run(t, "TYPE", "thezset"))

	// These have no Redis equivalent to agree with, so they answer with their
	// own name rather than a borrowed one.
	assert.Equal(t, "hll", run(t, "TYPE", "thehll"))
	assert.Equal(t, "cuckoo", run(t, "TYPE", "thecf"))

	assert.Equal(t, "none", run(t, "TYPE", "absent"),
		"Redis answers +none, and clients test for exactly that")
}

func TestKeysMatchesAcrossKeyspaces(t *testing.T) {
	ResetStores()
	run(t, "SET", "user:1", "a")
	run(t, "SET", "user:2", "b")
	run(t, "SADD", "user:3", "m")
	run(t, "SET", "other", "c")

	assert.ElementsMatch(t, []string{"user:1", "user:2", "user:3"},
		toStrings(run(t, "KEYS", "user:*")),
		"a set matching the pattern is as much a key as a string is")

	assert.ElementsMatch(t, []string{"user:1", "user:2", "user:3", "other"},
		toStrings(run(t, "KEYS", "*")))

	assert.Empty(t, toStrings(run(t, "KEYS", "nothing:*")))
}

// TestKeysTreatsSlashesAsOrdinary is the case Go's path.Match gets wrong, at
// the level of the command rather than the matcher.
func TestKeysTreatsSlashesAsOrdinary(t *testing.T) {
	ResetStores()
	run(t, "SET", "cache/user/1", "a")
	run(t, "SET", "cache/user/2", "b")
	run(t, "SET", "cache/post/1", "c")

	assert.Len(t, toStrings(run(t, "KEYS", "cache/*")), 3,
		"* has to cross a slash: a key is not a path")
	assert.ElementsMatch(t, []string{"cache/user/1", "cache/user/2"},
		toStrings(run(t, "KEYS", "cache/user/*")))
}

func TestKeysDoesNotListAnExpiredKey(t *testing.T) {
	ResetStores()
	run(t, "SET", "live", "v")
	run(t, "SET", "dying", "v", "PX", "1")
	waitPast(5)

	assert.Equal(t, []string{"live"}, toStrings(run(t, "KEYS", "*")),
		"KEYS must not show what GET would say was gone")
}

func TestMGetReadsSeveralKeysAndNilsTheRest(t *testing.T) {
	ResetStores()
	run(t, "SET", "a", "1")
	run(t, "SET", "b", "2")

	got, ok := run(t, "MGET", "a", "b").([]interface{})
	assert.True(t, ok, "MGET answers an array")
	assert.Equal(t, []interface{}{"1", "2"}, got)

	// Asserted on the wire rather than through Decode, which cannot tell a null
	// bulk string from an empty one - and the difference between them is the
	// whole point of this reply.
	assert.Equal(t, "*3\r\n$1\r\n1\r\n$-1\r\n$1\r\n2\r\n",
		string(rawReply(t, "MGET", "a", "absent", "b")),
		"a missing key is a null element, not a shorter array and not an empty string")
}

// TestMGetAnswersNilForAKeyOfAnotherType is the one place this server's
// stricter "one name, one thing" rule gives way to Redis's, deliberately.
func TestMGetAnswersNilForAKeyOfAnotherType(t *testing.T) {
	ResetStores()
	run(t, "SET", "str", "1")
	run(t, "SADD", "theset", "m")

	assert.Equal(t, "*2\r\n$1\r\n1\r\n$-1\r\n",
		string(rawReply(t, "MGET", "str", "theset")),
		"one key of the wrong type must not destroy the answer to the others")
}

func TestMSetWritesEveryPair(t *testing.T) {
	ResetStores()
	assert.Equal(t, "OK", run(t, "MSET", "a", "1", "b", "2", "c", "3"))
	assert.Equal(t, "1", run(t, "GET", "a"))
	assert.Equal(t, "2", run(t, "GET", "b"))
	assert.Equal(t, "3", run(t, "GET", "c"))
	assert.Equal(t, int64(3), run(t, "DBSIZE"))
}

func TestMSetRefusesAnOddNumberOfArguments(t *testing.T) {
	ResetStores()
	res, _ := run(t, "MSET", "a", "1", "b").(string)
	assert.Contains(t, res, "wrong number of arguments")
	assert.Equal(t, int64(0), run(t, "DBSIZE"), "and writes nothing")
}

// TestMSetChecksTheTypeOfEveryKeyNotOnlyTheFirst is why MSET needed a stride
// rather than being folded in with the commands whose every argument is a key.
func TestMSetChecksTheTypeOfEveryKeyNotOnlyTheFirst(t *testing.T) {
	ResetStores()
	run(t, "SADD", "theset", "m")

	res, _ := run(t, "MSET", "fine", "1", "theset", "2").(string)
	assert.Contains(t, res, "WRONGTYPE",
		"the offending key is second, and checking only the first would miss it")

	assert.Equal(t, int64(0), run(t, "EXISTS", "fine"),
		"a refused MSET writes none of its pairs, not the ones before the bad key")
	assert.Equal(t, "set", run(t, "TYPE", "theset"))
}

// TestMSetDoesNotTypeCheckItsValues is the other half of the stride being two.
// A value that happens to equal the name of a set is just a value.
func TestMSetDoesNotTypeCheckItsValues(t *testing.T) {
	ResetStores()
	run(t, "SADD", "theset", "m")

	assert.Equal(t, "OK", run(t, "MSET", "k", "theset"),
		"'theset' here is a value, and values have no type to be wrong about")
	assert.Equal(t, "theset", run(t, "GET", "k"))
}

func TestFlushDBEmptiesEveryKeyspace(t *testing.T) {
	ResetStores()
	run(t, "SET", "s", "v")
	run(t, "SADD", "theset", "m")
	run(t, "ZADD", "thezset", "1", "m")
	run(t, "PFADD", "thehll", "m")
	assert.Equal(t, int64(4), run(t, "DBSIZE"))

	assert.Equal(t, "OK", run(t, "FLUSHDB"))
	assert.Equal(t, int64(0), run(t, "DBSIZE"))
	assert.Empty(t, toStrings(run(t, "KEYS", "*")))
	assert.Equal(t, int64(0), run(t, "EXISTS", "s", "theset", "thezset", "thehll"))
}

func TestFlushDBFreesTheMemoryItAccountedFor(t *testing.T) {
	ResetStores()
	for _, k := range []string{"a", "b", "c"} {
		run(t, "SET", k, "some value of a length")
	}
	assert.Positive(t, data_structure.TotalMemUsed(), "the keys cost something")

	run(t, "FLUSHDB")
	assert.Equal(t, uint64(0), data_structure.TotalMemUsed(),
		"an empty keyspace holds nothing, and an unsigned counter that did not "+
			"balance would wrap rather than go negative")
}

// TestFlushDBRefusesArguments: Redis takes ASYNC and SYNC here, and neither
// means anything on a server with no background free.
func TestFlushDBRefusesArguments(t *testing.T) {
	ResetStores()
	run(t, "SET", "k", "v")
	res, _ := run(t, "FLUSHDB", "ASYNC").(string)
	assert.Contains(t, res, "wrong number of arguments")
	assert.Equal(t, int64(1), run(t, "DBSIZE"), "and flushes nothing")
}

// TestMSetAndFlushDBSurviveARestart: both change the dataset, so both have to
// be in the log. A write missing from writeCommands loses data silently at the
// next restart, which is the failure that shows up furthest from its cause.
func TestMSetAndFlushDBSurviveARestart(t *testing.T) {
	path := withAOF(t, func() {
		run(t, "MSET", "a", "1", "b", "2", "c", "3")
	})
	restart(t, path)

	assert.Equal(t, "1", run(t, "GET", "a"))
	assert.Equal(t, "2", run(t, "GET", "b"))
	assert.Equal(t, "3", run(t, "GET", "c"))
	assert.Equal(t, int64(3), run(t, "DBSIZE"))
}

func TestFlushDBIsNotUndoneByARestart(t *testing.T) {
	path := withAOF(t, func() {
		run(t, "MSET", "a", "1", "b", "2")
		run(t, "SADD", "theset", "m")
		run(t, "FLUSHDB")
		run(t, "SET", "after", "kept")
	})
	restart(t, path)

	// The log holds the writes from before the flush as well as the flush
	// itself, so replaying it has to arrive at empty-then-one-key rather than
	// at everything ever written.
	assert.Equal(t, int64(1), run(t, "DBSIZE"),
		"replaying a log containing FLUSHDB must not restore what it removed")
	assert.Equal(t, "kept", run(t, "GET", "after"))
	assert.Equal(t, int64(0), run(t, "EXISTS", "a", "b", "theset"))
}

// TestFlushDBDuringARewriteDoesNotResurrectTheKeyspace is the case cmdFLUSHDB
// marks its keys dirty for.
//
// A rewrite walks the keyspace a slice at a time, writing each key it finds
// into a new log. If FLUSHDB empties the keyspace halfway through, the keys the
// walk already wrote are gone but the new log still says to create them - so
// unless the rewrite is told they changed, finishing it produces a log that
// restores everything FLUSHDB just deleted.
func TestFlushDBDuringARewriteDoesNotResurrectTheKeyspace(t *testing.T) {
	path := withAOF(t, func() {
		// More than rewriteChunk, so the walk takes several cycles and can be
		// caught in the middle of one.
		for i := 0; i < 5000; i++ {
			run(t, "SET", "key:"+strconv.Itoa(i), "value")
		}
		assert.NoError(t, StartRewrite())

		// One slice, so the walk has written some keys and not others. This is
		// the state the bug needs: a partially written new log.
		assert.True(t, stepRewrite(t), "the walk should have more to do")

		run(t, "FLUSHDB")
		// The server flushes the log between executing a cycle's commands and
		// writing its replies, so by the next slice the flush has reached the
		// old file - the file the rewrite is about to rename away. Without this
		// the record of FLUSHDB is still sitting in the buffer when the swap
		// happens and lands in the new file by luck, which is the test passing
		// for a reason that has nothing to do with what it is testing.
		assert.NoError(t, FlushAOF())

		for stepRewrite(t) {
		}
	})

	restart(t, path)
	assert.Equal(t, int64(0), run(t, "DBSIZE"),
		"a rewrite interrupted by FLUSHDB must not bring the keyspace back")
	// Length rather than the slice itself: when this fails it fails by
	// thousands of keys, and printing them all buries the number that matters.
	assert.Len(t, toStrings(run(t, "KEYS", "*")), 0)
}
