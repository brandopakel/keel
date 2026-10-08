package core

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTheServersEngineRefusesWritesWhileItsLogFails: the server's engine,
// which its loop drives, handles a failed write of its log as Redis does
// under everysec and no. The flush that fails reports it, but it is retried,
// not latched: write commands and PING are answered with Redis's MISCONF
// error and do not run, reads are served, and INFO says err. A transaction
// with a write is refused, at the write or at EXEC. Once the disk heals, the
// next flush writes what was kept and writes are accepted again. Under
// always, where Redis exits, the failure latches, and the server stops on it.
func TestTheServersEngineRefusesWritesWhileItsLogFails(t *testing.T) {
	t.Parallel()
	for _, policy := range []FsyncPolicy{FsyncEverySec, FsyncNever, FsyncAlways} {
		t.Run(string(policy), func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "keel.aof")
			e := newTestEngine(t, Options{Fsync: policy})
			require.NoError(t, e.OpenAOF(path))
			disk := failDisk(e)
			assert.Equal(t, "+OK\r\n", string(rawReplyOn(t, e, "SET", "before", "v")))
			require.NoError(t, e.FlushAOF())
			disk.writes.Store(true)

			assert.Equal(t, "+OK\r\n", string(rawReplyOn(t, e, "SET", "inflight", "v")), "it ran before the failure was known")
			require.ErrorIs(t, e.FlushAOF(), disk.err)
			if policy == FsyncAlways {
				assert.False(t, e.LogRetrying())
				require.ErrorIs(t, e.aof.failed, disk.err, "latched: the server stops on it")
				return
			}
			require.True(t, e.LogRetrying())
			require.NoError(t, e.aof.failed)
			const misconf = "-MISCONF Errors writing to the AOF file: no space left on the test's disk\r\n"
			for _, cmd := range [][]string{{"SET", "denied", "v"}, {"INCR", "n"}, {"PING"}} {
				assert.Equal(t, misconf, string(rawReplyOn(t, e, cmd[0], cmd[1:]...)), "%v", cmd)
			}
			assert.Equal(t, "$-1\r\n", string(rawReplyOn(t, e, "GET", "denied")), "refused writes do not run")
			assert.Equal(t, "$1\r\nv\r\n", string(rawReplyOn(t, e, "GET", "before")), "reads are served")
			assert.Contains(t, string(rawReplyOn(t, e, "INFO", "persistence")), "aof_last_write_status:err")

			var tx *Transaction
			transact := func(name string, args ...string) string {
				var w replyWriter
				var err error
				tx, err = e.Transact(tx, &Command{Cmd: name, Args: args}, &w, nil)
				require.NoError(t, err)
				return string(w.b)
			}
			assert.Equal(t, "+OK\r\n", transact("MULTI"))
			assert.Equal(t, misconf, transact("SET", "queued", "v"), "refused as it is queued, as Redis refuses it")
			assert.Equal(t, "-EXECABORT Transaction discarded because of previous errors.\r\n", transact("EXEC"))

			disk.writes.Store(false)
			require.NoError(t, e.FlushAOF(), "the retry succeeds")
			assert.False(t, e.LogRetrying())
			assert.Equal(t, "+PONG\r\n", string(rawReplyOn(t, e, "PING")))
			assert.Equal(t, "+OK\r\n", string(rawReplyOn(t, e, "SET", "after", "v")), "writes are accepted again")
			assert.Contains(t, string(rawReplyOn(t, e, "INFO", "persistence")), "aof_last_write_status:ok")
			require.NoError(t, e.FlushAOF())
			require.NoError(t, e.CloseAOF())

			again := openTestEngine(t, Options{AppendOnly: true, AppendFilename: path})
			for _, key := range []string{"before", "inflight", "after"} {
				assert.Equal(t, "$1\r\nv\r\n", mustDo(t, again, "GET", key), key)
			}
			assert.Equal(t, "$-1\r\n", mustDo(t, again, "GET", "denied"))
		})
	}
}

// TestAFailureThatBeginsDuringATransactionAbortsItsExec: a write queued while
// the log was healthy is checked again at EXEC, which a failed log refuses
// whole, as Redis's processCommand refuses an EXEC whose block writes
// (server.c 4547-4548) and aborts it (execCommandAbort); a PING queued then
// is not, as Redis checks only EXEC's writes.
func TestAFailureThatBeginsDuringATransactionAbortsItsExec(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		queued []string
		want   string
	}{
		{[]string{"SET", "k", "v"}, "-EXECABORT Transaction discarded because of: MISCONF Errors writing to the AOF file: no space left on the test's disk\r\n"},
		{[]string{"PING"}, "*1\r\n+PONG\r\n"},
	} {
		t.Run(c.queued[0], func(t *testing.T) {
			t.Parallel()
			e := newTestEngine(t, Options{Fsync: FsyncEverySec})
			require.NoError(t, e.OpenAOF(filepath.Join(t.TempDir(), "keel.aof")))
			disk := failDisk(e)
			var tx *Transaction
			transact := func(name string, args ...string) string {
				var w replyWriter
				var err error
				tx, err = e.Transact(tx, &Command{Cmd: name, Args: args}, &w, nil)
				require.NoError(t, err)
				return string(w.b)
			}
			assert.Equal(t, "+OK\r\n", transact("MULTI"))
			assert.Equal(t, "+QUEUED\r\n", transact(c.queued[0], c.queued[1:]...))
			disk.writes.Store(true)
			rawReplyOn(t, e, "SET", "inflight", "v")
			require.Error(t, e.FlushAOF())
			require.True(t, e.LogRetrying())
			assert.Equal(t, c.want, transact("EXEC"))
		})
	}
}

// TestTheCauseIsInRedissWords: MISCONF's cause is strerror's text for an
// errno, which Redis prints, as Go words it with its first letter
// capitalised; a short write is ENOSPC, as Redis records it. The four errnos
// a full or failing disk gives are pinned, in each platform's own words: on
// macOS EDQUOT is "Disc quota exceeded", as its strerror spells it. Any other
// failure is Go's own text, the one place the text cannot be Redis's.
func TestTheCauseIsInRedissWords(t *testing.T) {
	t.Parallel()
	quota := "Disk quota exceeded"
	if runtime.GOOS == "darwin" {
		quota = "Disc quota exceeded"
	}
	for _, c := range []struct {
		err  error
		want string
	}{
		{&fs.PathError{Op: "write", Path: "keel.aof", Err: syscall.ENOSPC}, "No space left on device"},
		{&fs.PathError{Op: "write", Path: "keel.aof", Err: syscall.EIO}, "Input/output error"},
		{&fs.PathError{Op: "write", Path: "keel.aof", Err: syscall.EROFS}, "Read-only file system"},
		{&fs.PathError{Op: "write", Path: "keel.aof", Err: syscall.EDQUOT}, quota},
		{io.ErrShortWrite, "No space left on device"},
		{fmt.Errorf("sync keel.aof: %w", syscall.EIO), "Input/output error"},
		{errors.New("no space left on the test's disk"), "no space left on the test's disk"},
	} {
		got := persistenceError{c.err}.Error()
		assert.Equal(t, "MISCONF Errors writing to the AOF file: "+c.want, got, "%v", c.err)
		assert.ErrorIs(t, persistenceError{c.err}, ErrPersistence)
		assert.ErrorIs(t, persistenceError{c.err}, c.err)
	}
	// The words pinned above are the C library's strerror, which Redis
	// prints: asked of it through Python's os.strerror, where there is one.
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Log("no python3 to ask strerror; the table above pins its words")
		return
	}
	for _, errno := range []syscall.Errno{syscall.ENOSPC, syscall.EIO, syscall.EROFS, syscall.EDQUOT} {
		out, err := exec.Command(python, "-c", fmt.Sprintf("import os; print(os.strerror(%d))", int(errno))).Output()
		require.NoError(t, err)
		assert.Equal(t, strings.TrimSpace(string(out)), causeText(errno), "strerror(%d)", int(errno))
	}
}
