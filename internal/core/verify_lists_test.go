package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// This file sorts after every file of the package's code, so that its tests
// move no handler in the test binary the paired command-path job measures.

// TestListReadsCountWhereRedisLooks: LLEN and LINDEX look their key up
// before they read anything else, so each counts a hit or a miss whatever
// follows; LRANGE reads start and stop first, and counts nothing for a
// malformed one, as lrangeCommand returns before its lookup; a key of
// another type is a hit once the arguments have passed. LPOS reads its
// options first, as LRANGE does. Pops, trims, sets, inserts, removals and
// moves look up to write, and count nothing.
func TestListReadsCountWhereRedisLooks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		cmds [][]string
		want [2]uint64
	}{
		{"LRANGE", [][]string{{"LRANGE", "list", "0", "-1"}, {"LRANGE", "nosuch", "0", "-1"}}, [2]uint64{1, 1}},
		{"LRANGE malformed", [][]string{{"LRANGE", "list", "x", "1"}, {"LRANGE", "nosuch", "0", "x"}, {"LRANGE", "str", "x", "1"}}, [2]uint64{0, 0}},
		{"LRANGE of another type", [][]string{{"LRANGE", "str", "0", "-1"}}, [2]uint64{1, 0}},
		{"LINDEX and LLEN", [][]string{{"LINDEX", "list", "x"}, {"LINDEX", "nosuch", "x"}, {"LLEN", "str"}, {"LLEN", "nosuch"}}, [2]uint64{2, 2}},
		{"writes", [][]string{{"LPOP", "nosuch"}, {"RPOP", "list", "0"}, {"LTRIM", "nosuch", "0", "1"}, {"LSET", "list", "0", "v"}, {"LPUSH", "list", "v"}}, [2]uint64{0, 0}},
		{"LPOS", [][]string{{"LPOS", "list", "a"}, {"LPOS", "nosuch", "a", "COUNT", "0"}, {"LPOS", "list", "a", "RANK", "0"}, {"LPOS", "str", "a", "RANK", "0"}}, [2]uint64{1, 1}},
		{"LPOS of another type", [][]string{{"LPOS", "str", "a"}}, [2]uint64{1, 0}},
		{"the new writes", [][]string{{"LPUSHX", "list", "v"}, {"RPUSHX", "nosuch", "v"}, {"LINSERT", "list", "BEFORE", "a", "v"},
			{"LREM", "nosuch", "0", "a"}, {"LMOVE", "list", "d", "LEFT", "RIGHT"}, {"RPOPLPUSH", "nosuch", "d"},
			{"LMOVEM", "list", "d", "LEFT", "LEFT", "COUNT", "2", "OBO"}, {"LMPOP", "2", "nosuch", "d", "LEFT"}}, [2]uint64{0, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newTestEngine(t, Options{})
			runOn(t, e, "SET", "str", "abc")
			runOn(t, e, "RPUSH", "list", "a", "b")
			e.totals = commandTotals{}
			for _, c := range tc.cmds {
				runOn(t, e, c[0], c[1:]...)
			}
			assert.Equal(t, tc.want, lookups(e), "hits and misses")
		})
	}
}

// listWrites are the list writes a replica must refuse, one of each and of
// each form, with a list and a missing key to aim at.
var listWrites = [][]string{
	{"LPUSH", "list", "v"}, {"RPUSH", "list", "v", "w"}, {"LPUSH", "nosuch", "v"},
	{"LPOP", "list"}, {"RPOP", "list"}, {"LPOP", "list", "2"}, {"RPOP", "nosuch", "0"},
	{"LSET", "list", "0", "v"}, {"LTRIM", "list", "0", "0"}, {"LTRIM", "nosuch", "0", "-1"},
}

// TestReplicaRefusesEveryListWrite: a replica refuses each list write with
// READONLY, on every form, the converted command language included, and the
// list it would have written is untouched.
func TestReplicaRefusesEveryListWrite(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "RPUSH", "list", "a", "b")
	reconfigure(t, e, func(o *Options) { o.ReplicaOf = "test-primary:6379" })
	for _, cmd := range listWrites {
		assert.Equal(t, "-READONLY You can't write against a read only replica.\r\n",
			string(rawReplyOn(t, e, cmd[0], cmd[1:]...)), "%v", cmd)
	}
	l, ok := e.listStore.Peek("list")
	assert.True(t, ok)
	assert.Equal(t, []string{"a", "b"}, l.All(), "no write reached the list")
	_, ok = e.listStore.Peek("nosuch")
	assert.False(t, ok)
}
