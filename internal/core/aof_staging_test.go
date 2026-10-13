package core

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// stagingEngine is an engine with its log open, for the staging tests.
func stagingEngine(t *testing.T) *Engine {
	t.Helper()
	e := newTestEngine(t, Options{Fsync: FsyncNever})
	require.NoError(t, e.OpenAOF(filepath.Join(t.TempDir(), "staging.aof")))
	return e
}

// TestStagedRecordsAreTheBytesAppendAOFCommandWrites: a staged record,
// integer parts included, is written byte for byte as the strings it stands
// for, both in the buffer and when it is drained in fragments.
func TestStagedRecordsAreTheBytesAppendAOFCommandWrites(t *testing.T) {
	t.Parallel()
	for _, n := range []int64{0, 7, -7, 99, 100, 1_700_000_000_123, math.MaxInt64, math.MinInt64, -1} {
		for _, value := range []string{"v", strings.Repeat("x", maxAOFTranscriptBytes)} {
			e := stagingEngine(t)
			want := stagingEngine(t)
			e.aofBegin("SET")
			e.aofRecord("SET", "k", value)
			e.aofRecordInt("PEXPIREAT", "k", n)
			e.appendStaged()
			want.aofBegin("SET")
			want.appendAOFCommand("SET", "k", value)
			want.appendAOFCommand("PEXPIREAT", "k", strconv.FormatInt(n, 10))
			require.NoError(t, e.FlushAOF())
			require.NoError(t, want.FlushAOF())
			require.Equal(t, readLog(t, want), readLog(t, e), "n=%d len(value)=%d", n, len(value))
		}
	}
}

func readLog(t *testing.T, e *Engine) string {
	t.Helper()
	require.NoError(t, e.aof.file.Sync())
	b, err := os.ReadFile(e.aof.path)
	require.NoError(t, err)
	return string(b)
}

// TestStagingAllocatesNothingOnceGrown: staging a record, with a string or
// an integer part, allocates nothing once the staging area has grown, so a
// typed operation can record every write it makes without a cost per write.
func TestStagingAllocatesNothingOnceGrown(t *testing.T) {
	// Not parallel: AllocsPerRun refuses to run beside other tests.
	e := stagingEngine(t)
	key, value := "key", "value"
	e.aofBegin("SET")
	e.aofRecord("SET", key, value)
	e.aofRecordInt("PEXPIREAT", key, 1_700_000_000_123)
	allocs := testing.AllocsPerRun(1000, func() {
		e.clearStaged()
		e.aofRecord("SET", key, value)
		e.aofRecordInt("PEXPIREAT", key, 1_700_000_000_123)
	})
	require.Zero(t, allocs)
	require.Len(t, e.aof.staged, 8, "two headers and six parts")
	e.clearStaged()
	require.Zero(t, retainedStagingBytes(e.aof.staged))
}
