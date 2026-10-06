package core

import (
	"math"
	"runtime"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPushesReportTheNewLength(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.Equal(t, int64(3), runOn(t, e, "RPUSH", "l", "a", "b", "c"))
	assert.Equal(t, int64(4), runOn(t, e, "LPUSH", "l", "z"))
	assert.Equal(t, []string{"z", "a", "b", "c"}, toStrings(runOn(t, e, "LRANGE", "l", "0", "-1")))
}

// TestLPushOfSeveralReversesThem: LPUSH a b c leaves c at the head, because
// each element is pushed onto the front in turn. It reads like a bug and is
// what Redis does.
func TestLPushOfSeveralReversesThem(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "LPUSH", "l", "a", "b", "c")
	assert.Equal(t, []string{"c", "b", "a"}, toStrings(runOn(t, e, "LRANGE", "l", "0", "-1")))
}

func TestLRangeClampsRatherThanFailing(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "RPUSH", "l", "a", "b", "c", "d", "e")

	assert.Equal(t, []string{"b", "c"}, toStrings(runOn(t, e, "LRANGE", "l", "1", "2")))
	assert.Equal(t, []string{"d", "e"}, toStrings(runOn(t, e, "LRANGE", "l", "-2", "-1")))
	assert.Len(t, toStrings(runOn(t, e, "LRANGE", "l", "-100", "100")), 5,
		"a range wider than the list is the whole list, which is why 0 -1 is the idiom")
	assert.Empty(t, toStrings(runOn(t, e, "LRANGE", "l", "3", "1")), "backwards is empty")
	assert.Empty(t, toStrings(runOn(t, e, "LRANGE", "l", "10", "20")), "past the end is empty")
	assert.Empty(t, toStrings(runOn(t, e, "LRANGE", "absent", "0", "-1")))
}

func TestLIndexCountsFromEitherEnd(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "RPUSH", "l", "a", "b", "c")
	assert.Equal(t, "a", runOn(t, e, "LINDEX", "l", "0"))
	assert.Equal(t, "c", runOn(t, e, "LINDEX", "l", "-1"))
	assert.Equal(t, "$-1\r\n", string(rawReplyOn(t, e, "LINDEX", "l", "99")),
		"past the end is a null, not an error")
	assert.Equal(t, "$-1\r\n", string(rawReplyOn(t, e, "LINDEX", "absent", "0")))
}

func TestLSetRefusesWhatItCannotAddress(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "RPUSH", "l", "a", "b")

	assert.Equal(t, "OK", runOn(t, e, "LSET", "l", "0", "first"))
	assert.Equal(t, "OK", runOn(t, e, "LSET", "l", "-1", "last"))
	assert.Equal(t, []string{"first", "last"}, toStrings(runOn(t, e, "LRANGE", "l", "0", "-1")))

	res, _ := runOn(t, e, "LSET", "l", "99", "nope").(string)
	assert.Contains(t, res, "index out of range")
	res, _ = runOn(t, e, "LSET", "absent", "0", "v").(string)
	assert.Contains(t, res, "no such key", "LSET does not create a list")
}

// TestPopWithACountAnswersAnArrayAndWithoutOneAnElement is the distinction that
// costs a client a type error when it is got wrong.
func TestPopWithACountAnswersAnArrayAndWithoutOneAnElement(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "RPUSH", "l", "a", "b", "c")

	assert.Equal(t, "$1\r\na\r\n", string(rawReplyOn(t, e, "LPOP", "l")),
		"no count is one element")
	assert.Equal(t, "*2\r\n$1\r\nc\r\n$1\r\nb\r\n", string(rawReplyOn(t, e, "RPOP", "l", "2")),
		"a count is an array, even for one")
}

// TestPopOnAMissingKeyAnswersTheRightKindOfNull.
//
// A null array and a null bulk string are different replies. Checked against
// Redis 8.10.1: *-1 for the counted form, $-1 for the bare one. Answering $-1
// for both is a type error in any client decoding the counted reply into a
// list, and it is what this did until the difference was measured.
func TestPopOnAMissingKeyAnswersTheRightKindOfNull(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.Equal(t, "$-1\r\n", string(rawReplyOn(t, e, "LPOP", "absent")))
	assert.Equal(t, "*-1\r\n", string(rawReplyOn(t, e, "LPOP", "absent", "2")))
	assert.Equal(t, "*-1\r\n", string(rawReplyOn(t, e, "RPOP", "absent", "2")))
}

func TestPoppingMoreThanIsThereTakesWhatThereIs(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "RPUSH", "l", "a", "b")
	assert.Equal(t, []string{"a", "b"}, toStrings(runOn(t, e, "LPOP", "l", "5")))
	assert.Equal(t, int64(0), runOn(t, e, "EXISTS", "l"))
}

// TestEmptyingAListRemovesTheKey: Redis has no empty list, and neither can
// this - an empty one would be written by the rewrite as an RPUSH with no
// values, which is a syntax error on replay.
func TestEmptyingAListRemovesTheKey(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "RPUSH", "l", "only")
	assert.Equal(t, int64(1), runOn(t, e, "EXISTS", "l"))

	assert.Equal(t, "only", runOn(t, e, "LPOP", "l"))
	assert.Equal(t, int64(0), runOn(t, e, "EXISTS", "l"), "the last element takes the key with it")
	assert.Equal(t, "none", runOn(t, e, "TYPE", "l"))
	assert.Equal(t, int64(0), runOn(t, e, "LLEN", "l"))
}

func TestListIsItsOwnTypeAcrossTheKeyspace(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "RPUSH", "l", "a")

	assert.Equal(t, "list", runOn(t, e, "TYPE", "l"), "the word Redis uses")
	assert.Equal(t, int64(1), runOn(t, e, "EXISTS", "l"))
	assert.Equal(t, int64(1), runOn(t, e, "DBSIZE"))
	assert.Equal(t, []string{"l"}, toStrings(runOn(t, e, "KEYS", "*")))

	for _, cmd := range [][]string{{"GET", "l"}, {"SADD", "l", "m"}, {"HSET", "l", "f", "v"}} {
		res, _ := runOn(t, e, cmd[0], cmd[1:]...).(string)
		assert.Contains(t, res, "WRONGTYPE", "%s against a list", cmd[0])
	}
	assert.Equal(t, int64(1), runOn(t, e, "DEL", "l"))
}

func TestListCommandsRefuseAKeyOfAnotherType(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "SET", "str", "v")
	for _, cmd := range [][]string{
		{"RPUSH", "str", "x"}, {"LPUSH", "str", "x"}, {"LPOP", "str"},
		{"LLEN", "str"}, {"LINDEX", "str", "0"}, {"LRANGE", "str", "0", "-1"},
		{"LSET", "str", "0", "v"},
	} {
		res, _ := runOn(t, e, cmd[0], cmd[1:]...).(string)
		assert.Contains(t, res, "WRONGTYPE", "%s against a string", cmd[0])
	}
}

func TestListCountsTowardTheMemoryBudget(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	before := e.space.TotalMemUsed()
	runOn(t, e, "RPUSH", "l", "an element of some length")
	assert.Greater(t, e.space.TotalMemUsed(), before)

	runOn(t, e, "DEL", "l")
	assert.Equal(t, before, e.space.TotalMemUsed(),
		"and gives back exactly what it took")
}

func TestListSurvivesARestart(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := withAOFOn(t, e, func() {
		runOn(t, e, "RPUSH", "l", "a", "b", "c")
		runOn(t, e, "LPUSH", "l", "z")
		runOn(t, e, "LPOP", "l")
		runOn(t, e, "LSET", "l", "0", "changed")
	})
	restartOn(t, e, path)

	assert.Equal(t, "list", runOn(t, e, "TYPE", "l"))
	assert.Equal(t, []string{"changed", "b", "c"}, toStrings(runOn(t, e, "LRANGE", "l", "0", "-1")),
		"pushes, pops and sets all replay in order")
}

// TestARewrittenLogRebuildsTheListInOrder: order is the whole content of a
// list, so a rewrite that got it backwards would still restore the right
// elements and the wrong list.
func TestARewrittenLogRebuildsTheListInOrder(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := withAOFOn(t, e, func() {
		for i := 0; i < 100; i++ {
			runOn(t, e, "RPUSH", "l", strconv.Itoa(i))
		}
		for i := 0; i < 50; i++ {
			runOn(t, e, "LPOP", "l")
		}
		assert.NoError(t, e.RewriteAOF())
	})
	restartOn(t, e, path)

	got := toStrings(runOn(t, e, "LRANGE", "l", "0", "-1"))
	assert.Len(t, got, 50)
	assert.Equal(t, "50", got[0], "150 commands collapse to one RPUSH holding the answer")
	assert.Equal(t, "99", got[49])
}

// TestAHugePopCountAllocatesOnlyWhatTheListHolds.
//
// count arrives from the client and was the capacity of a make(), so LPOP l
// 2147483647 asked for 34GB and took the process out - one command, on a
// connection needing no authentication because there is none. Measured for the
// commit that fixed it: the allocation follows the requested count, not the
// list, at 16 bytes per requested slot.
//
// The same shape as the SPOP bug this server already fixed once, where asking
// for more members than the set held spun the event loop forever.
func TestAHugePopCountAllocatesOnlyWhatTheListHolds(t *testing.T) {
	// Not parallel: it reads the process's heap statistics, which a test
	// running beside it would move.
	e := newTestEngine(t, Options{})
	runOn(t, e, "RPUSH", "l", "a", "b")

	// TotalAlloc, which is cumulative and never decreases, rather than
	// HeapAlloc. The allocation this is about is transient: the slice is
	// unreachable the moment the command returns, so a HeapAlloc reading taken
	// afterwards has already had it collected and shows nothing. That version
	// of this test passed with the clamp removed, which is the only reason this
	// note exists.
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	got := toStrings(runOn(t, e, "LPOP", "l", strconv.Itoa(math.MaxInt32)))

	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	assert.Equal(t, []string{"a", "b"}, got, "and it still answers what is there")
	assert.Equal(t, int64(0), runOn(t, e, "EXISTS", "l"))

	allocated := after.TotalAlloc - before.TotalAlloc
	assert.Less(t, allocated, uint64(1<<20),
		"popping two elements allocated %d bytes; the count is the client's, so "+
			"the list has to be the bound", allocated)
}

func TestAHugePopCountOnEveryEnd(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "RPUSH", "l", "a")
	assert.Equal(t, []string{"a"}, toStrings(runOn(t, e, "RPOP", "l", strconv.Itoa(math.MaxInt32))))

	e.resetStores()
	runOn(t, e, "RPUSH", "l", "a")
	assert.Equal(t, []string{"a"}, toStrings(runOn(t, e, "LPOP", "l", strconv.Itoa(math.MaxInt64))),
		"a count that overflows int32 is still just a count")
}

// TestARewriteRebuildsAListWhoseHeadHasWrapped.
//
// The ring's head moves with every pop, so a list that has been pushed and
// popped enough wraps past the end of its buffer. All() walks modulo the
// capacity, and a rewrite reads it - so an off-by-one there would write the
// list back in the wrong order, which no test with an unwrapped head can see.
func TestARewriteRebuildsAListWhoseHeadHasWrapped(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := withAOFOn(t, e, func() {
		for i := 0; i < 100; i++ {
			runOn(t, e, "RPUSH", "l", strconv.Itoa(i))
		}
		// Rotate far enough that the head is past the end of the buffer several
		// times over, leaving the elements physically split across it.
		for round := 0; round < 350; round++ {
			v := runOn(t, e, "LPOP", "l")
			runOn(t, e, "RPUSH", "l", v.(string))
		}
		assert.NoError(t, e.RewriteAOF())
	})

	before := toStrings(runOn(t, e, "LRANGE", "l", "0", "-1"))
	restartOn(t, e, path)

	assert.Equal(t, before, toStrings(runOn(t, e, "LRANGE", "l", "0", "-1")),
		"a wrapped list rebuilds in the order it was actually in")
	assert.Equal(t, "50", runOn(t, e, "LINDEX", "l", "0"),
		"350 rotations of a hundred elements leaves 50 at the head")
}
