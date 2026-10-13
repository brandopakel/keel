package core

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// This file sorts after every file of the package's code, so that the test
// below, compiled into package core, moves none of the handlers in the test
// binary the paired command-path job measures (docs/phase4-family-recipe.md,
// step 5).

// TestStagedArgumentRecordsAreTheBytesAppendAOFCommandWrites: a record
// staged as a name and an argument list, or as a name, a key and an
// argument list, is written as the strings it stands for, and staging one
// allocates nothing once the area has grown.
func TestStagedArgumentRecordsAreTheBytesAppendAOFCommandWrites(t *testing.T) {
	for _, args := range [][]string{nil, {"a"}, {"a", "1", "b", "2"}} {
		e := stagingEngine(t)
		want := stagingEngine(t)
		e.aofBegin("MSET")
		e.aofRecordArgs("MSET", args)
		e.aofRecordKeyArgs("RPUSH", "list", args)
		e.appendStaged()
		want.aofBegin("MSET")
		want.appendAOFCommand("MSET", args...)
		want.appendAOFCommand("RPUSH", append([]string{"list"}, args...)...)
		require.NoError(t, e.FlushAOF())
		require.NoError(t, want.FlushAOF())
		require.Equal(t, readLog(t, want), readLog(t, e), "%q", args)
	}
	// Not parallel: AllocsPerRun refuses to run beside other tests.
	e := stagingEngine(t)
	values := []string{"a", "b", "c"}
	e.aofBegin("RPUSH")
	e.aofRecordKeyArgs("RPUSH", "list", values)
	allocs := testing.AllocsPerRun(1000, func() {
		e.clearStaged()
		e.aofRecordKeyArgs("RPUSH", "list", values)
	})
	require.Zero(t, allocs)
	require.Len(t, e.aof.staged, 6, "a header and five parts")
}
