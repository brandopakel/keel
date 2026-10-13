package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// This file sorts after every file of the package's code, so that its tests
// move no handler in the test binary the paired command-path job measures.

// sketchWrites are the sketch writes a replica must refuse, one of each and
// of each form, with a sketch of each kind and a missing key to aim at.
var sketchWrites = [][]string{
	{"PFADD", "hll", "x"}, {"PFADD", "nosuch"}, {"PFMERGE", "hll", "nosuch"},
	{"BF.RESERVE", "nosuch", "0.01", "100"}, {"BF.ADD", "bf", "x"}, {"BF.MADD", "bf", "x", "y"},
	{"CF.RESERVE", "nosuch", "1000"}, {"CF.ADD", "cf", "x"}, {"CF.ADDNX", "cf", "x"}, {"CF.DEL", "cf", "a"},
	{"CMS.INITBYDIM", "nosuch", "10", "5"}, {"CMS.INITBYPROB", "nosuch", "0.01", "0.01"},
	{"CMS.INCRBY", "cms", "x", "1"},
	{"MORRIS.INITBYDIM", "nosuch", "10", "5"}, {"MORRIS.INITBYPROB", "nosuch", "0.01", "0.01"},
	{"MORRIS.INCRBY", "mor", "x", "1"},
}

// TestReplicaRefusesEverySketchWrite: a replica refuses each sketch write
// with READONLY, on every form, the converted command language included, and
// nothing it would have written changes.
func TestReplicaRefusesEverySketchWrite(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	for _, setup := range [][]string{
		{"PFADD", "hll", "a"}, {"BF.ADD", "bf", "a"}, {"CF.ADD", "cf", "a"},
		{"CMS.INITBYDIM", "cms", "10", "5"}, {"MORRIS.INITBYDIM", "mor", "10", "5"},
	} {
		runOn(t, e, setup[0], setup[1:]...)
	}
	before := map[string][]byte{}
	for _, key := range []string{"hll", "bf", "cf", "cms", "mor"} {
		before[key], _ = e.dumpKey(key)
	}
	reconfigure(t, e, func(o *Options) { o.ReplicaOf = "test-primary:6379" })
	for _, cmd := range sketchWrites {
		assert.Equal(t, "-READONLY You can't write against a read only replica.\r\n",
			string(rawReplyOn(t, e, cmd[0], cmd[1:]...)), "%v", cmd)
	}
	for key, dump := range before {
		after, _ := e.dumpKey(key)
		assert.Equal(t, dump, after, "%s is untouched", key)
	}
	_, held := e.space.OwnerOf("nosuch")
	assert.False(t, held, "nothing was created")
}
