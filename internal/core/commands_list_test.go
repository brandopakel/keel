package core

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/brandopakel/keel/internal/data_structure"
)

func TestPushesReportTheNewLength(t *testing.T) {
	ResetStores()
	assert.Equal(t, int64(3), run(t, "RPUSH", "l", "a", "b", "c"))
	assert.Equal(t, int64(4), run(t, "LPUSH", "l", "z"))
	assert.Equal(t, []string{"z", "a", "b", "c"}, toStrings(run(t, "LRANGE", "l", "0", "-1")))
}

// TestLPushOfSeveralReversesThem: LPUSH a b c leaves c at the head, because
// each element is pushed onto the front in turn. It reads like a bug and is
// what Redis does.
func TestLPushOfSeveralReversesThem(t *testing.T) {
	ResetStores()
	run(t, "LPUSH", "l", "a", "b", "c")
	assert.Equal(t, []string{"c", "b", "a"}, toStrings(run(t, "LRANGE", "l", "0", "-1")))
}

func TestLRangeClampsRatherThanFailing(t *testing.T) {
	ResetStores()
	run(t, "RPUSH", "l", "a", "b", "c", "d", "e")

	assert.Equal(t, []string{"b", "c"}, toStrings(run(t, "LRANGE", "l", "1", "2")))
	assert.Equal(t, []string{"d", "e"}, toStrings(run(t, "LRANGE", "l", "-2", "-1")))
	assert.Len(t, toStrings(run(t, "LRANGE", "l", "-100", "100")), 5,
		"a range wider than the list is the whole list, which is why 0 -1 is the idiom")
	assert.Empty(t, toStrings(run(t, "LRANGE", "l", "3", "1")), "backwards is empty")
	assert.Empty(t, toStrings(run(t, "LRANGE", "l", "10", "20")), "past the end is empty")
	assert.Empty(t, toStrings(run(t, "LRANGE", "absent", "0", "-1")))
}

func TestLIndexCountsFromEitherEnd(t *testing.T) {
	ResetStores()
	run(t, "RPUSH", "l", "a", "b", "c")
	assert.Equal(t, "a", run(t, "LINDEX", "l", "0"))
	assert.Equal(t, "c", run(t, "LINDEX", "l", "-1"))
	assert.Equal(t, "$-1\r\n", string(rawReply(t, "LINDEX", "l", "99")),
		"past the end is a null, not an error")
	assert.Equal(t, "$-1\r\n", string(rawReply(t, "LINDEX", "absent", "0")))
}

func TestLSetRefusesWhatItCannotAddress(t *testing.T) {
	ResetStores()
	run(t, "RPUSH", "l", "a", "b")

	assert.Equal(t, "OK", run(t, "LSET", "l", "0", "first"))
	assert.Equal(t, "OK", run(t, "LSET", "l", "-1", "last"))
	assert.Equal(t, []string{"first", "last"}, toStrings(run(t, "LRANGE", "l", "0", "-1")))

	res, _ := run(t, "LSET", "l", "99", "nope").(string)
	assert.Contains(t, res, "index out of range")
	res, _ = run(t, "LSET", "absent", "0", "v").(string)
	assert.Contains(t, res, "no such key", "LSET does not create a list")
}

// TestPopWithACountAnswersAnArrayAndWithoutOneAnElement is the distinction that
// costs a client a type error when it is got wrong.
func TestPopWithACountAnswersAnArrayAndWithoutOneAnElement(t *testing.T) {
	ResetStores()
	run(t, "RPUSH", "l", "a", "b", "c")

	assert.Equal(t, "$1\r\na\r\n", string(rawReply(t, "LPOP", "l")),
		"no count is one element")
	assert.Equal(t, "*2\r\n$1\r\nc\r\n$1\r\nb\r\n", string(rawReply(t, "RPOP", "l", "2")),
		"a count is an array, even for one")
}

// TestPopOnAMissingKeyAnswersTheRightKindOfNull.
//
// A null array and a null bulk string are different replies. Checked against
// Redis 8.10.1: *-1 for the counted form, $-1 for the bare one. Answering $-1
// for both is a type error in any client decoding the counted reply into a
// list, and it is what this did until the difference was measured.
func TestPopOnAMissingKeyAnswersTheRightKindOfNull(t *testing.T) {
	ResetStores()
	assert.Equal(t, "$-1\r\n", string(rawReply(t, "LPOP", "absent")))
	assert.Equal(t, "*-1\r\n", string(rawReply(t, "LPOP", "absent", "2")))
	assert.Equal(t, "*-1\r\n", string(rawReply(t, "RPOP", "absent", "2")))
}

func TestPoppingMoreThanIsThereTakesWhatThereIs(t *testing.T) {
	ResetStores()
	run(t, "RPUSH", "l", "a", "b")
	assert.Equal(t, []string{"a", "b"}, toStrings(run(t, "LPOP", "l", "5")))
	assert.Equal(t, int64(0), run(t, "EXISTS", "l"))
}

// TestEmptyingAListRemovesTheKey: Redis has no empty list, and neither can
// this - an empty one would be written by the rewrite as an RPUSH with no
// values, which is a syntax error on replay.
func TestEmptyingAListRemovesTheKey(t *testing.T) {
	ResetStores()
	run(t, "RPUSH", "l", "only")
	assert.Equal(t, int64(1), run(t, "EXISTS", "l"))

	assert.Equal(t, "only", run(t, "LPOP", "l"))
	assert.Equal(t, int64(0), run(t, "EXISTS", "l"), "the last element takes the key with it")
	assert.Equal(t, "none", run(t, "TYPE", "l"))
	assert.Equal(t, int64(0), run(t, "LLEN", "l"))
}

func TestListIsItsOwnTypeAcrossTheKeyspace(t *testing.T) {
	ResetStores()
	run(t, "RPUSH", "l", "a")

	assert.Equal(t, "list", run(t, "TYPE", "l"), "the word Redis uses")
	assert.Equal(t, int64(1), run(t, "EXISTS", "l"))
	assert.Equal(t, int64(1), run(t, "DBSIZE"))
	assert.Equal(t, []string{"l"}, toStrings(run(t, "KEYS", "*")))

	for _, cmd := range [][]string{{"GET", "l"}, {"SADD", "l", "m"}, {"HSET", "l", "f", "v"}} {
		res, _ := run(t, cmd[0], cmd[1:]...).(string)
		assert.Contains(t, res, "WRONGTYPE", "%s against a list", cmd[0])
	}
	assert.Equal(t, int64(1), run(t, "DEL", "l"))
}

func TestListCommandsRefuseAKeyOfAnotherType(t *testing.T) {
	ResetStores()
	run(t, "SET", "str", "v")
	for _, cmd := range [][]string{
		{"RPUSH", "str", "x"}, {"LPUSH", "str", "x"}, {"LPOP", "str"},
		{"LLEN", "str"}, {"LINDEX", "str", "0"}, {"LRANGE", "str", "0", "-1"},
		{"LSET", "str", "0", "v"},
	} {
		res, _ := run(t, cmd[0], cmd[1:]...).(string)
		assert.Contains(t, res, "WRONGTYPE", "%s against a string", cmd[0])
	}
}

func TestListCountsTowardTheMemoryBudget(t *testing.T) {
	ResetStores()
	before := data_structure.TotalMemUsed()
	run(t, "RPUSH", "l", "an element of some length")
	assert.Greater(t, data_structure.TotalMemUsed(), before)

	run(t, "DEL", "l")
	assert.Equal(t, before, data_structure.TotalMemUsed(),
		"and gives back exactly what it took")
}

func TestListSurvivesARestart(t *testing.T) {
	path := withAOF(t, func() {
		run(t, "RPUSH", "l", "a", "b", "c")
		run(t, "LPUSH", "l", "z")
		run(t, "LPOP", "l")
		run(t, "LSET", "l", "0", "changed")
	})
	restart(t, path)

	assert.Equal(t, "list", run(t, "TYPE", "l"))
	assert.Equal(t, []string{"changed", "b", "c"}, toStrings(run(t, "LRANGE", "l", "0", "-1")),
		"pushes, pops and sets all replay in order")
}

// TestARewrittenLogRebuildsTheListInOrder: order is the whole content of a
// list, so a rewrite that got it backwards would still restore the right
// elements and the wrong list.
func TestARewrittenLogRebuildsTheListInOrder(t *testing.T) {
	path := withAOF(t, func() {
		for i := 0; i < 100; i++ {
			run(t, "RPUSH", "l", strconv.Itoa(i))
		}
		for i := 0; i < 50; i++ {
			run(t, "LPOP", "l")
		}
		assert.NoError(t, RewriteAOF())
	})
	restart(t, path)

	got := toStrings(run(t, "LRANGE", "l", "0", "-1"))
	assert.Len(t, got, 50)
	assert.Equal(t, "50", got[0], "150 commands collapse to one RPUSH holding the answer")
	assert.Equal(t, "99", got[49])
}
