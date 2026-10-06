package core

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
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
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "SET", "s", "v")
	runOn(t, e, "SADD", "theset", "m")
	runOn(t, e, "ZADD", "thezset", "1", "m")
	runOn(t, e, "PFADD", "thehll", "m")

	assert.Equal(t, int64(1), runOn(t, e, "EXISTS", "s"))
	assert.Equal(t, int64(1), runOn(t, e, "EXISTS", "theset"),
		"a set is a key, and EXISTS is not a string command")
	assert.Equal(t, int64(1), runOn(t, e, "EXISTS", "thezset"))
	assert.Equal(t, int64(1), runOn(t, e, "EXISTS", "thehll"))
	assert.Equal(t, int64(0), runOn(t, e, "EXISTS", "absent"))

	assert.Equal(t, int64(4), runOn(t, e, "EXISTS", "s", "theset", "thezset", "thehll"))
	assert.Equal(t, int64(2), runOn(t, e, "EXISTS", "s", "absent", "theset"))
}

// TestExistsCountsRepeatsRepeatedly is Redis's behaviour from 3.0 onwards, and
// the one people find surprising often enough to be worth pinning.
func TestExistsCountsRepeatsRepeatedly(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "SET", "k", "v")
	assert.Equal(t, int64(3), runOn(t, e, "EXISTS", "k", "k", "k"))
	assert.Equal(t, int64(0), runOn(t, e, "EXISTS", "nope", "nope"))
}

func TestExistsDoesNotSeeAnExpiredKey(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	// A TTL long enough that the first EXISTS lands inside it on a slow
	// runner: with one millisecond, the macOS CI job saw the key expire
	// between the SET and the check.
	runOn(t, e, "SET", "k", "v", "PX", "200")
	assert.Equal(t, int64(1), runOn(t, e, "EXISTS", "k"))
	waitPast(250)
	assert.Equal(t, int64(0), runOn(t, e, "EXISTS", "k"),
		"a key past its TTL exists no more than a deleted one")
}

func TestTypeNamesTheKeyspaceHoldingTheKey(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "SET", "s", "v")
	runOn(t, e, "SADD", "theset", "m")
	runOn(t, e, "ZADD", "thezset", "1", "m")
	runOn(t, e, "PFADD", "thehll", "m")
	runOn(t, e, "CF.ADD", "thecf", "m")

	// The first three are the words Redis uses, so a client switching on the
	// reply behaves the same against either server.
	assert.Equal(t, "string", runOn(t, e, "TYPE", "s"))
	assert.Equal(t, "set", runOn(t, e, "TYPE", "theset"))
	assert.Equal(t, "zset", runOn(t, e, "TYPE", "thezset"))

	// These have no Redis equivalent to agree with, so they answer with their
	// own name rather than a borrowed one.
	assert.Equal(t, "hll", runOn(t, e, "TYPE", "thehll"))
	assert.Equal(t, "cuckoo", runOn(t, e, "TYPE", "thecf"))

	assert.Equal(t, "none", runOn(t, e, "TYPE", "absent"),
		"Redis answers +none, and clients test for exactly that")
}

func TestKeysMatchesAcrossKeyspaces(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "SET", "user:1", "a")
	runOn(t, e, "SET", "user:2", "b")
	runOn(t, e, "SADD", "user:3", "m")
	runOn(t, e, "SET", "other", "c")

	assert.ElementsMatch(t, []string{"user:1", "user:2", "user:3"},
		toStrings(runOn(t, e, "KEYS", "user:*")),
		"a set matching the pattern is as much a key as a string is")

	assert.ElementsMatch(t, []string{"user:1", "user:2", "user:3", "other"},
		toStrings(runOn(t, e, "KEYS", "*")))

	assert.Empty(t, toStrings(runOn(t, e, "KEYS", "nothing:*")))
}

// TestKeysTreatsSlashesAsOrdinary is the case Go's path.Match gets wrong, at
// the level of the command rather than the matcher.
func TestKeysTreatsSlashesAsOrdinary(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "SET", "cache/user/1", "a")
	runOn(t, e, "SET", "cache/user/2", "b")
	runOn(t, e, "SET", "cache/post/1", "c")

	assert.Len(t, toStrings(runOn(t, e, "KEYS", "cache/*")), 3,
		"* has to cross a slash: a key is not a path")
	assert.ElementsMatch(t, []string{"cache/user/1", "cache/user/2"},
		toStrings(runOn(t, e, "KEYS", "cache/user/*")))
}

func TestKeysDoesNotListAnExpiredKey(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "SET", "live", "v")
	runOn(t, e, "SET", "dying", "v", "PX", "1")
	waitPast(5)

	assert.Equal(t, []string{"live"}, toStrings(runOn(t, e, "KEYS", "*")),
		"KEYS must not show what GET would say was gone")
}

func TestMGetReadsSeveralKeysAndNilsTheRest(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "SET", "a", "1")
	runOn(t, e, "SET", "b", "2")

	got, ok := runOn(t, e, "MGET", "a", "b").([]interface{})
	assert.True(t, ok, "MGET answers an array")
	assert.Equal(t, []interface{}{"1", "2"}, got)

	// Asserted on the wire rather than through Decode, which cannot tell a null
	// bulk string from an empty one - and the difference between them is the
	// whole point of this reply.
	assert.Equal(t, "*3\r\n$1\r\n1\r\n$-1\r\n$1\r\n2\r\n",
		string(rawReplyOn(t, e, "MGET", "a", "absent", "b")),
		"a missing key is a null element, not a shorter array and not an empty string")
}

// TestMGetAnswersNilForAKeyOfAnotherType is the one place this server's
// stricter "one name, one thing" rule gives way to Redis's, deliberately.
func TestMGetAnswersNilForAKeyOfAnotherType(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "SET", "str", "1")
	runOn(t, e, "SADD", "theset", "m")

	assert.Equal(t, "*2\r\n$1\r\n1\r\n$-1\r\n",
		string(rawReplyOn(t, e, "MGET", "str", "theset")),
		"one key of the wrong type must not destroy the answer to the others")
}

func TestMSetWritesEveryPair(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.Equal(t, "OK", runOn(t, e, "MSET", "a", "1", "b", "2", "c", "3"))
	assert.Equal(t, "1", runOn(t, e, "GET", "a"))
	assert.Equal(t, "2", runOn(t, e, "GET", "b"))
	assert.Equal(t, "3", runOn(t, e, "GET", "c"))
	assert.Equal(t, int64(3), runOn(t, e, "DBSIZE"))
}

func TestMSetRefusesAnOddNumberOfArguments(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	res, _ := runOn(t, e, "MSET", "a", "1", "b").(string)
	assert.Contains(t, res, "wrong number of arguments")
	assert.Equal(t, int64(0), runOn(t, e, "DBSIZE"), "and writes nothing")
}

// TestMSetReplacesEveryKeyWhateverItsType, as Redis's MSET does, the second
// key included: a stride of two is still what finds it.
func TestMSetReplacesEveryKeyWhateverItsType(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "SADD", "theset", "m")

	assert.Equal(t, "OK", runOn(t, e, "MSET", "fine", "1", "theset", "2"))
	assert.Equal(t, "string", runOn(t, e, "TYPE", "theset"))
	assert.Equal(t, "2", runOn(t, e, "GET", "theset"))
	assert.Equal(t, "1", runOn(t, e, "GET", "fine"))
	assert.Equal(t, 2, e.space.TotalKeys(), "the set is gone, not shadowed")
}

// TestMSetDoesNotTypeCheckItsValues is the other half of the stride being two.
// A value that happens to equal the name of a set is just a value.
func TestMSetDoesNotTypeCheckItsValues(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "SADD", "theset", "m")

	assert.Equal(t, "OK", runOn(t, e, "MSET", "k", "theset"),
		"'theset' here is a value, and values have no type to be wrong about")
	assert.Equal(t, "theset", runOn(t, e, "GET", "k"))
}

func TestFlushDBEmptiesEveryKeyspace(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "SET", "s", "v")
	runOn(t, e, "SADD", "theset", "m")
	runOn(t, e, "ZADD", "thezset", "1", "m")
	runOn(t, e, "PFADD", "thehll", "m")
	assert.Equal(t, int64(4), runOn(t, e, "DBSIZE"))

	assert.Equal(t, "OK", runOn(t, e, "FLUSHDB"))
	assert.Equal(t, int64(0), runOn(t, e, "DBSIZE"))
	assert.Empty(t, toStrings(runOn(t, e, "KEYS", "*")))
	assert.Equal(t, int64(0), runOn(t, e, "EXISTS", "s", "theset", "thezset", "thehll"))
}

func TestFlushDBFreesTheMemoryItAccountedFor(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	for _, k := range []string{"a", "b", "c"} {
		runOn(t, e, "SET", k, "some value of a length")
	}
	assert.Positive(t, e.space.TotalMemUsed(), "the keys cost something")

	runOn(t, e, "FLUSHDB")
	assert.Equal(t, uint64(0), e.space.TotalMemUsed(),
		"an empty keyspace holds nothing, and an unsigned counter that did not "+
			"balance would wrap rather than go negative")
}

// TestFlushDBArguments: SYNC and ASYNC are Redis's and both flush before the
// reply; anything else is Redis's syntax error and flushes nothing.
func TestFlushDBArguments(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "SET", "k", "v")
	for _, args := range [][]string{{"BAD"}, {"SYNC", "ASYNC"}} {
		assert.Equal(t, "-ERR syntax error\r\n", string(rawReplyOn(t, e, "FLUSHDB", args...)))
		assert.Equal(t, int64(1), runOn(t, e, "DBSIZE"), "and flushes nothing")
	}
	for _, mode := range []string{"ASYNC", "sync"} {
		runOn(t, e, "SET", "k", "v")
		assert.Equal(t, "OK", runOn(t, e, "FLUSHDB", mode))
		assert.Equal(t, int64(0), runOn(t, e, "DBSIZE"))
	}
}

// TestMSetAndFlushDBSurviveARestart: both change the dataset, so both have to
// be in the log. A write missing from writeCommands loses data silently at the
// next restart, which is the failure that shows up furthest from its cause.
func TestMSetAndFlushDBSurviveARestart(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := withAOFOn(t, e, func() {
		runOn(t, e, "MSET", "a", "1", "b", "2", "c", "3")
	})
	restartOn(t, e, path)

	assert.Equal(t, "1", runOn(t, e, "GET", "a"))
	assert.Equal(t, "2", runOn(t, e, "GET", "b"))
	assert.Equal(t, "3", runOn(t, e, "GET", "c"))
	assert.Equal(t, int64(3), runOn(t, e, "DBSIZE"))
}

func TestFlushDBIsNotUndoneByARestart(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := withAOFOn(t, e, func() {
		runOn(t, e, "MSET", "a", "1", "b", "2")
		runOn(t, e, "SADD", "theset", "m")
		runOn(t, e, "FLUSHDB")
		runOn(t, e, "SET", "after", "kept")
	})
	restartOn(t, e, path)

	// The log holds the writes from before the flush as well as the flush
	// itself, so replaying it has to arrive at empty-then-one-key rather than
	// at everything ever written.
	assert.Equal(t, int64(1), runOn(t, e, "DBSIZE"),
		"replaying a log containing FLUSHDB must not restore what it removed")
	assert.Equal(t, "kept", runOn(t, e, "GET", "after"))
	assert.Equal(t, int64(0), runOn(t, e, "EXISTS", "a", "b", "theset"))
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
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := withAOFOn(t, e, func() {
		// More than rewriteChunk, so the walk takes several cycles and can be
		// caught in the middle of one.
		for i := 0; i < 5000; i++ {
			runOn(t, e, "SET", "key:"+strconv.Itoa(i), "value")
		}
		assert.NoError(t, e.StartRewrite())

		// One slice, so the walk has written some keys and not others. This is
		// the state the bug needs: a partially written new log.
		assert.True(t, stepRewriteOn(t, e), "the walk should have more to do")

		runOn(t, e, "FLUSHDB")
		// The server flushes the log between executing a cycle's commands and
		// writing its replies, so by the next slice the flush has reached the
		// old file - the file the rewrite is about to rename away. Without this
		// the record of FLUSHDB is still sitting in the buffer when the swap
		// happens and lands in the new file by luck, which is the test passing
		// for a reason that has nothing to do with what it is testing.
		assert.NoError(t, e.FlushAOF())

		for stepRewriteOn(t, e) {
		}
	})

	restartOn(t, e, path)
	assert.Equal(t, int64(0), runOn(t, e, "DBSIZE"),
		"a rewrite interrupted by FLUSHDB must not bring the keyspace back")
	// Length rather than the slice itself: when this fails it fails by
	// thousands of keys, and printing them all buries the number that matters.
	assert.Len(t, toStrings(runOn(t, e, "KEYS", "*")), 0)
}
