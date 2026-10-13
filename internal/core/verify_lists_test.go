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
// another type is a hit once the arguments have passed. Pops, trims and sets
// look up to write, and count nothing.
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
