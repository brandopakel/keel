package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/brandopakel/keel/internal/testlock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reopenedCopy opens a copy of the log at path, in a directory of its own, as
// the next start after a crash at this moment would open it, and says how
// many torn tails that start repaired.
func reopenedCopy(t *testing.T, path string) (*Engine, int) {
	t.Helper()
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	dir := t.TempDir()
	copied := filepath.Join(dir, "keel.aof")
	require.NoError(t, os.WriteFile(copied, body, 0o644))
	e := openTestEngine(t, Options{AppendOnly: true, AppendFilename: copied})
	torn := 0
	for name := range filesIn(t, dir) {
		if strings.HasPrefix(name, ".keel-torn-tail-") {
			torn++
		}
	}
	return e, torn
}

// logSize is the size of the file at path.
func logSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	return info.Size()
}

// TestAShortWriteIsCutBackOffTheLog: a write of whole records that fails
// partway is cut back off the file, as Redis 8.10.1 truncates a short write to
// the last whole size (aof.c 1515-1526), so the log ends with its last whole
// record and a crash then needs no repair; the whole buffer is still to be
// written. Under everysec and no the retry writes it once the disk heals.
// Where the cut fails, what was written stays and the rest follows it, as
// Redis does when it cannot truncate (aof.c 1547-1551), and a crash then
// leaves a torn tail for the next start to repair. The server's engine,
// which stops at a failed write, cuts it too.
func TestAShortWriteIsCutBackOffTheLog(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		policy   FsyncPolicy
		cutFails bool
	}{
		{"everysec", FsyncEverySec, false},
		{"no", FsyncNever, false},
		{"always", FsyncAlways, false},
		{"everysec, the cut failing", FsyncEverySec, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "keel.aof")
			o := Options{AppendOnly: true, AppendFilename: path, Fsync: c.policy}
			e := openTestEngine(t, o)
			disk := failDisk(e)
			if c.cutFails {
				holding(e, func() {
					e.aofTruncate = func(*os.File, int64) error { return errors.New("the test's disk cannot truncate") }
				})
			}
			mustDo(t, e, "SET", "before", "v")
			whole := logSize(t, path)
			disk.writes.Store(true)

			reply, err := do(context.Background(), e, "SET", "inflight", "v")
			if c.policy == FsyncAlways {
				require.ErrorIs(t, err, ErrPersistence, "under always the failure latches")
			} else {
				require.NoError(t, err)
				assert.Equal(t, "+OK\r\n", reply)
			}
			copied, torn := reopenedCopy(t, path)
			assert.Equal(t, "$1\r\nv\r\n", mustDo(t, copied, "GET", "before"))
			if c.cutFails {
				assert.Greater(t, logSize(t, path), whole, "what was written stays")
				assert.Equal(t, 1, torn, "a crash now leaves a torn tail")
			} else {
				assert.Equal(t, whole, logSize(t, path), "the short write is cut back off")
				assert.Zero(t, torn, "a crash now leaves a whole log")
			}
			assert.Equal(t, "$-1\r\n", mustDo(t, copied, "GET", "inflight"), "not yet written, as in Redis")
			if c.policy == FsyncAlways {
				return
			}

			disk.writes.Store(false)
			eventually(t, e, "the retried write succeeded", func() bool { return !e.logRetrying() })
			require.NoError(t, e.Close())
			again, torn := reopenedCopy(t, path)
			assert.Zero(t, torn)
			assert.Equal(t, "$1\r\nv\r\n", mustDo(t, again, "GET", "before"))
			assert.Equal(t, "$1\r\nv\r\n", mustDo(t, again, "GET", "inflight"), "written whole by the retry")
		})
	}
}

// TestTheServersShortWriteIsCutBackOffTheLog: the server's engine has no
// maintenance goroutine and stops at a failed write, but it cuts a short
// write off first, as Redis does before it exits, so the next start finds a
// whole log and repairs nothing.
func TestTheServersShortWriteIsCutBackOffTheLog(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "keel.aof")
	e := newTestEngine(t, Options{})
	require.NoError(t, e.OpenAOF(path))
	disk := failDisk(e)
	runOn(t, e, "SET", "before", "v")
	require.NoError(t, e.FlushAOF())
	whole := logSize(t, path)
	disk.writes.Store(true)
	runOn(t, e, "SET", "lost", "v")
	require.ErrorIs(t, e.FlushAOF(), disk.err)
	assert.Equal(t, whole, logSize(t, path), "the short write is cut back off")
	copied, torn := reopenedCopy(t, path)
	assert.Zero(t, torn)
	assert.Equal(t, "$1\r\nv\r\n", mustDo(t, copied, "GET", "before"))
}

// TestAShortWriteInTheMiddleOfARecordStays: a large record is drained to the
// log as it is encoded, so once its first part is on disk the file ends in
// the middle of it, and a later part's short write cannot be cut back to a
// whole record that is not there. What was written stays, the rest of the
// record is kept for the retry, as Redis does when it cannot truncate (aof.c
// 1547-1551), and the log is whole once the retry succeeds.
func TestAShortWriteInTheMiddleOfARecordStays(t *testing.T) {
	t.Parallel()
	// About 20 MiB of logs, taken before the engine's directory.
	testlock.HoldDiskHeavy(t)
	path := filepath.Join(t.TempDir(), "keel.aof")
	o := Options{AppendOnly: true, AppendFilename: path, Fsync: FsyncEverySec}
	e := openTestEngine(t, o)
	// The second write after the failure is armed fails: the first is the
	// large record's first drain, the second its next.
	var writes, failFrom atomic.Int64
	failing := errors.New("no space left on the test's disk")
	holding(e, func() {
		write := e.aofWrite
		e.aofWrite = func(f *os.File, b []byte) (int, error) {
			if from := failFrom.Load(); from > 0 && writes.Add(1) >= from {
				n, _ := write(f, b[:len(b)/2])
				return n, failing
			}
			return write(f, b)
		}
	})
	mustDo(t, e, "SET", "before", "v")
	whole := logSize(t, path)
	failFrom.Store(2)

	large := strings.Repeat("x", 2*maxAOFTranscriptBytes+128<<10)
	reply, err := do(context.Background(), e, "SET", "large", large)
	require.NoError(t, err)
	assert.Equal(t, "+OK\r\n", reply)
	assert.Greater(t, logSize(t, path), whole+maxAOFTranscriptBytes, "the first drain and the failed write's part stay")
	copied, torn := reopenedCopy(t, path)
	assert.Equal(t, 1, torn, "a crash now leaves the record's start as a torn tail")
	assert.Equal(t, "$1\r\nv\r\n", mustDo(t, copied, "GET", "before"))

	failFrom.Store(0)
	eventually(t, e, "the retried write succeeded", func() bool { return !e.logRetrying() })
	require.NoError(t, e.Close())
	again, torn := reopenedCopy(t, path)
	assert.Zero(t, torn)
	assert.Equal(t, "$"+strconv.Itoa(len(large))+"\r\n"+large+"\r\n", mustDo(t, again, "GET", "large"))
}

// TestAShortWriteAfterARewriteIsCutBackExactly: the cut goes back to where the
// failed write began, which it takes from the file itself, so a log a rewrite
// has replaced, whose size no count of appends describes, is cut back to its
// last whole record and no further.
func TestAShortWriteAfterARewriteIsCutBackExactly(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "keel.aof")
	o := Options{AppendOnly: true, AppendFilename: path, Fsync: FsyncEverySec}
	e := openTestEngine(t, o)
	for i := range 200 {
		mustDo(t, e, "SET", "k"+strconv.Itoa(i%20), strconv.Itoa(i))
	}
	assert.Contains(t, mustDo(t, e, "BGREWRITEAOF"), "rewriting started")
	eventually(t, e, "the rewrite finished", func() bool { return !e.RewriteActive() })
	mustDo(t, e, "SET", "after", "rewrite")
	whole := logSize(t, path)

	disk := failDisk(e)
	disk.writes.Store(true)
	reply, err := do(context.Background(), e, "SET", "inflight", "v")
	require.NoError(t, err)
	assert.Equal(t, "+OK\r\n", reply)
	assert.Equal(t, whole, logSize(t, path), "cut back to where the write began")
	copied, torn := reopenedCopy(t, path)
	assert.Zero(t, torn)
	assert.Equal(t, "$7\r\nrewrite\r\n", mustDo(t, copied, "GET", "after"))
	assert.Equal(t, "$3\r\n199\r\n", mustDo(t, copied, "GET", "k19"))

	disk.writes.Store(false)
	eventually(t, e, "the retried write succeeded", func() bool { return !e.logRetrying() })
	require.NoError(t, e.Close())
	again, torn := reopenedCopy(t, path)
	assert.Zero(t, torn)
	assert.Equal(t, "$1\r\nv\r\n", mustDo(t, again, "GET", "inflight"))
}
