package core

import (
	"bytes"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A failed rewrite must leave the server serving from the log it would have
// replaced, as Redis's does. These tests fail each step of a rewrite in turn
// and then check what Redis checks: the failure is reported in INFO and the
// log, the temporary file is gone, writes are still accepted into the old log
// and replay after a restart, and the next rewrite works.

type logCapture struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.b.Write(p)
}

func (c *logCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.b.String()
}

// captureLog collects the log for one test. The rewrite worker can log from
// its own goroutine, so the buffer is locked.
func captureLog(t *testing.T) *logCapture {
	t.Helper()
	c := &logCapture{}
	prev, flags := log.Writer(), log.Flags()
	log.SetOutput(c)
	t.Cleanup(func() { log.SetOutput(prev); log.SetFlags(flags) })
	return c
}

func infoPersistence(t *testing.T) string {
	t.Helper()
	return infoPersistenceOn(t, defaultEngine)
}

// infoPersistenceOn is infoPersistence on e.
func infoPersistenceOn(t *testing.T, e *Engine) string {
	t.Helper()
	return string(rawReplyOn(t, e, "INFO", "persistence"))
}

func persistenceField(t *testing.T, e *Engine, name string) string {
	t.Helper()
	for _, line := range strings.Split(infoPersistenceOn(t, e), "\r\n") {
		if value, ok := strings.CutPrefix(line, name+":"); ok {
			return value
		}
	}
	t.Fatalf("INFO persistence has no %s", name)
	return ""
}

// driveRewrite runs the loop's part until the rewrite ends, requiring every
// flush to succeed: nothing a rewrite does to its own file is the log's error.
func driveRewrite(t *testing.T) {
	t.Helper()
	driveRewriteOn(t, defaultEngine)
}

// driveRewriteOn is driveRewrite on e.
func driveRewriteOn(t *testing.T, e *Engine) {
	t.Helper()
	for n := 0; e.RewriteActive() && n < 10000; n++ {
		require.NoError(t, e.FlushAOF(), "a rewrite's failure must never become the log's")
		waitForRewriteSyncOn(t, e)
		if e.aof.syncPending != nil {
			e.pollAOFSync(true)
		}
	}
	require.False(t, e.RewriteActive(), "the rewrite did not end")
}

// seconds reads a whole-second INFO field. Redis counts these as the
// difference of two clock readings in seconds, so a short rewrite can read 1.
func seconds(t *testing.T, e *Engine, name string) int {
	t.Helper()
	n, err := strconv.Atoi(persistenceField(t, e, name))
	require.NoError(t, err)
	return n
}

func sameFile(t *testing.T, f *os.File, path string) bool {
	t.Helper()
	a, err := f.Stat()
	require.NoError(t, err)
	b, err := os.Stat(path)
	require.NoError(t, err)
	return os.SameFile(a, b)
}

func TestFailedRewriteKeepsServingFromTheOldLog(t *testing.T) {
	// Not parallel: it captures the process's log output, which a test
	// running beside it would write to as well.
	e := newTestEngine(t, Options{})
	injected := errors.New("injected")
	cases := []struct {
		name string
		// tail installs the fault once the snapshot is written and preflushed,
		// so it hits the synchronous dirty-tail phase.
		tail    bool
		install func()
		cause   string
	}{
		{name: "snapshot write ENOSPC", cause: "no space left on device", install: func() {
			e.rewriteFileWrite = func(*os.File, []byte) (int, error) { return 0, syscall.ENOSPC }
		}},
		{name: "snapshot short write", cause: io.ErrShortWrite.Error(), install: func() {
			e.rewriteFileWrite = func(f *os.File, body []byte) (int, error) { return f.Write(body[:min(3, len(body))]) }
		}},
		{name: "snapshot sync EIO", cause: "input/output error", install: func() {
			e.rewriteFileSync = func(*os.File) error { return syscall.EIO }
		}},
		{name: "dirty tail write EFBIG", tail: true, cause: "file too large", install: func() {
			e.rewriteFileWrite = func(*os.File, []byte) (int, error) { return 0, syscall.EFBIG }
		}},
		{name: "final sync EIO", tail: true, cause: "input/output error", install: func() {
			e.rewriteFileSync = func(*os.File) error { return syscall.EIO }
		}},
		{name: "open for appending", cause: "too many open files", install: func() {
			e.rewriteOpenLog = func(string) (*os.File, error) { return nil, syscall.EMFILE }
		}},
		{name: "rename", cause: "injected", install: func() {
			e.rewriteRename = func(oldPath, newPath string) error {
				return &os.LinkError{Op: "rename", Old: oldPath, New: newPath, Err: injected}
			}
		}},
	}
	for _, policy := range []FsyncPolicy{FsyncAlways, FsyncEverySec, FsyncNever} {
		for _, tc := range cases {
			t.Run(string(policy)+"/"+tc.name, func(t *testing.T) {
				logs := captureLog(t)
				e.resetStores()
				reconfigure(t, e, func(o *Options) { o.Fsync = policy })
				path := filepath.Join(t.TempDir(), "store.aof")
				require.NoError(t, e.OpenAOF(path))
				require.Equal(t, "ok", persistenceField(t, e, "aof_last_bgrewrite_status"))
				require.Equal(t, "-1", persistenceField(t, e, "aof_last_rewrite_time_sec"))

				require.Equal(t, "OK", runOn(t, e, "SET", "before", "1"))
				require.Equal(t, "OK", runOn(t, e, "SET", "big", strings.Repeat("v", 200<<10)))
				require.NoError(t, e.FlushAOF())
				if !tc.tail {
					tc.install()
				}
				require.Equal(t, "Background append only file rewriting started", runOn(t, e, "BGREWRITEAOF"))
				require.Equal(t, "ERR Background append only file rewriting already in progress", runOn(t, e, "BGREWRITEAOF"))
				require.Equal(t, "1", persistenceField(t, e, "aof_rewrite_in_progress"))
				require.LessOrEqual(t, seconds(t, e, "aof_current_rewrite_time_sec"), 1)
				require.GreaterOrEqual(t, seconds(t, e, "aof_current_rewrite_time_sec"), 0)
				require.Equal(t, "OK", runOn(t, e, "SET", "during", "2"))
				if tc.tail {
					// Install it once the snapshot's preflush has finished on
					// the worker and before the loop takes its result, so the
					// next cycle is the synchronous dirty tail and handoff.
					advanceToRewriteSync(t, e)
					waitForRewriteSyncOn(t, e)
					tc.install()
					require.Equal(t, "OK", runOn(t, e, "SET", "during-tail", "3"))
				}
				driveRewriteOn(t, e)

				// Reported as Redis reports it, and nothing else changed.
				require.Equal(t, "err", persistenceField(t, e, "aof_last_bgrewrite_status"))
				require.Equal(t, "1", persistenceField(t, e, "aof_rewrites_consecutive_failures"))
				require.Equal(t, "0", persistenceField(t, e, "aof_rewrite_in_progress"))
				require.Equal(t, "-1", persistenceField(t, e, "aof_current_rewrite_time_sec"))
				require.LessOrEqual(t, seconds(t, e, "aof_last_rewrite_time_sec"), 1)
				require.GreaterOrEqual(t, seconds(t, e, "aof_last_rewrite_time_sec"), 0)
				require.Equal(t, "0", persistenceField(t, e, "aof_rewrites"))
				require.Equal(t, "ok", persistenceField(t, e, "aof_last_write_status"))
				require.Contains(t, logs.String(), "Background AOF rewrite terminated with error: ")
				require.Contains(t, logs.String(), tc.cause)
				require.Nil(t, e.pendingRewriteIO)
				_, err := os.Stat(path + ".rewrite")
				require.True(t, os.IsNotExist(err), "the temporary file is removed")
				require.True(t, sameFile(t, e.aof.file, path), "the old log is still the one appended to")

				// Writes go on into the old log.
				require.Equal(t, "OK", runOn(t, e, "SET", "after", "4"))
				require.EqualValues(t, 5, runOn(t, e, "INCRBY", "counter", "5"))
				require.NoError(t, e.FlushAOF())
				if policy == FsyncAlways {
					body, err := os.ReadFile(path)
					require.NoError(t, err)
					require.Contains(t, string(body), string(appendCommand(nil, "SET", "after", "4")),
						"under always an accepted write is in the log before its reply")
				}

				// A later rewrite works, and reports so.
				restoreHooksNow(e)
				require.Equal(t, "Background append only file rewriting started", runOn(t, e, "BGREWRITEAOF"))
				driveRewriteOn(t, e)
				require.Equal(t, "ok", persistenceField(t, e, "aof_last_bgrewrite_status"))
				require.Equal(t, "0", persistenceField(t, e, "aof_rewrites_consecutive_failures"))
				require.Equal(t, "1", persistenceField(t, e, "aof_rewrites"))
				require.Contains(t, logs.String(), "Background AOF rewrite finished successfully")
				require.Equal(t, "OK", runOn(t, e, "SET", "final", "6"))
				require.NoError(t, e.CloseAOF())

				want := map[string]string{"before": "1", "during": "2", "after": "4", "counter": "5", "final": "6"}
				if tc.tail {
					want["during-tail"] = "3"
				}
				for i := 0; i < 2; i++ {
					restartOn(t, e, path)
					for key, value := range want {
						require.Equal(t, value, runOn(t, e, "GET", key), "%s after restart %d", key, i+1)
					}
					require.Equal(t, 200<<10, len(runOn(t, e, "GET", "big").(string)))
				}
			})
		}
	}
}

// restoreHooksNow puts the real steps back in the middle of a test, so the
// rewrite after a failure runs against the disk as it is.
func restoreHooksNow(e *Engine) {
	e.rewriteFileWrite = func(f *os.File, body []byte) (int, error) { return f.Write(body) }
	e.rewriteFileSync = func(f *os.File) error { return f.Sync() }
	e.rewriteOpenLog = func(path string) (*os.File, error) {
		return os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	}
	e.rewriteRename = os.Rename
	e.rewriteSyncDir = syncDir
}

// A start that fails is reported the way Redis reports a failed fork: the
// status is err, the consecutive-failure count is untouched, and BGREWRITEAOF
// gets Redis's generic refusal while the reason goes to the log.
func TestRewriteThatCannotStartIsReportedAsRedisReportsIt(t *testing.T) {
	// Not parallel: it captures the process's log output, which a test
	// running beside it would write to as well.
	e := newTestEngine(t, Options{})
	logs := captureLog(t)
	path := filepath.Join(t.TempDir(), "store.aof")
	require.NoError(t, e.OpenAOF(path))
	require.Equal(t, "OK", runOn(t, e, "SET", "k", "v"))
	require.NoError(t, os.Mkdir(path+".rewrite", 0o755)) // the temporary file cannot be created
	refusal := "ERR Can't execute an AOF background rewriting. Please check the server logs for more information."
	require.Equal(t, refusal, runOn(t, e, "BGREWRITEAOF"))
	require.False(t, e.RewriteActive())
	require.Equal(t, "err", persistenceField(t, e, "aof_last_bgrewrite_status"))
	require.Equal(t, "0", persistenceField(t, e, "aof_rewrites_consecutive_failures"))
	require.Contains(t, logs.String(), "Can't rewrite append only file in background: ")

	oldCount := e.keyCountForRewrite
	e.keyCountForRewrite = func() int { return RewriteKeyCeiling + 1 }
	require.Equal(t, refusal, runOn(t, e, "BGREWRITEAOF"))
	require.Contains(t, logs.String(), "rewrite limit")
	e.keyCountForRewrite = oldCount

	require.NoError(t, os.Remove(path+".rewrite"))
	require.Equal(t, "Background append only file rewriting started", runOn(t, e, "BGREWRITEAOF"))
	driveRewriteOn(t, e)
	require.Equal(t, "ok", persistenceField(t, e, "aof_last_bgrewrite_status"))
	require.NoError(t, e.CloseAOF())
	restartOn(t, e, path)
	require.Equal(t, "v", runOn(t, e, "GET", "k"))
}

// A transaction written to the log while a rewrite runs and then fails is in
// the old log whole, framed, and replays whole; a copy of the log cut inside
// its block replays none of it.
func TestTransactionDuringAFailedRewriteReplaysWhole(t *testing.T) {
	// Not parallel: it captures the process's log output, which a test
	// running beside it would write to as well.
	e := newTestEngine(t, Options{})
	captureLog(t)
	reconfigure(t, e, func(o *Options) { o.Fsync = FsyncAlways })
	path := filepath.Join(t.TempDir(), "store.aof")
	require.NoError(t, e.OpenAOF(path))
	release, started := make(chan struct{}), make(chan struct{})
	var once sync.Once
	e.rewriteFileWrite = func(*os.File, []byte) (int, error) {
		once.Do(func() { close(started) })
		<-release
		return 0, syscall.ENOSPC
	}
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
	})
	runOn(t, e, "SET", "left", "0")
	runOn(t, e, "SET", "right", "0")
	require.NoError(t, e.FlushAOF())
	require.NoError(t, e.StartRewrite())
	require.NoError(t, e.AdvanceRewrite())
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("the snapshot write did not start")
	}
	// The worker holds the write while a transaction runs and is logged.
	s := &session{t: t, e: e}
	s.send("MULTI")
	s.send("INCR", "left")
	s.send("SET", "middle", "inside")
	s.send("INCR", "right")
	require.Equal(t, "*3\r\n:1\r\n+OK\r\n:1\r\n", s.send("EXEC"))
	require.NoError(t, e.FlushAOF())
	require.True(t, e.RewriteActive())
	close(release)
	released = true
	driveRewriteOn(t, e)
	require.Equal(t, "err", persistenceField(t, e, "aof_last_bgrewrite_status"))
	// And one after the failure, in the same log.
	s.send("MULTI")
	s.send("INCR", "left")
	s.send("INCR", "right")
	require.Equal(t, "*2\r\n:2\r\n:2\r\n", s.send("EXEC"))
	require.NoError(t, e.FlushAOF())
	require.NoError(t, e.CloseAOF())

	body, err := os.ReadFile(path)
	require.NoError(t, err)
	first := "*1\r\n$5\r\nMULTI\r\n" + string(appendCommand(nil, "INCR", "left")) +
		string(appendCommand(nil, "SET", "middle", "inside")) + string(appendCommand(nil, "INCR", "right")) + "*1\r\n$4\r\nEXEC\r\n"
	require.Contains(t, string(body), first, "the block is in the old log whole")
	restartOn(t, e, path)
	require.Equal(t, "2", runOn(t, e, "GET", "left"))
	require.Equal(t, "2", runOn(t, e, "GET", "right"))
	require.Equal(t, "inside", runOn(t, e, "GET", "middle"))

	// Cut inside the second block: the first replays whole and the second not
	// at all, so the two counters never differ.
	cut := filepath.Join(t.TempDir(), "cut.aof")
	end := strings.LastIndex(string(body), "*1\r\n$4\r\nEXEC\r\n")
	require.NoError(t, os.WriteFile(cut, body[:end], 0o644))
	e.resetStores()
	_, err = e.LoadAOF(cut)
	require.True(t, IsTruncatedAOF(err), "an open block is a torn tail: %v", err)
	require.Equal(t, "1", runOn(t, e, "GET", "left"))
	require.Equal(t, "1", runOn(t, e, "GET", "right"))
}

// After the rename the new file is the log. A failed directory sync there is
// not a failed rewrite: the server keeps appending to the new file and tries
// the directory again before the log's next sync; if that fails too it is a
// failed sync of the log, which stops the server before anything appended to
// the new file is acknowledged.
func TestDirectorySyncAfterTheRenameIsRetriedBeforeTheNextSync(t *testing.T) {
	// Not parallel: it captures the process's log output, which a test
	// running beside it would write to as well.
	e := newTestEngine(t, Options{})
	for _, mode := range []string{"retry succeeds", "retry fails", "retry fails, worker appends"} {
		t.Run(mode, func(t *testing.T) {
			logs := captureLog(t)
			e.resetStores()
			reconfigure(t, e, func(o *Options) { o.Fsync = FsyncAlways })
			path := filepath.Join(t.TempDir(), "store.aof")
			require.NoError(t, e.OpenAOF(path))
			runOn(t, e, "SET", "before", "1")
			require.NoError(t, e.FlushAOF())
			calls := 0
			e.rewriteSyncDir = func(dir string) error {
				calls++
				if calls == 1 || mode != "retry succeeds" {
					return syscall.EIO
				}
				return syncDir(dir)
			}
			require.Equal(t, "Background append only file rewriting started", runOn(t, e, "BGREWRITEAOF"))
			runOn(t, e, "SET", "during", "2")
			driveRewriteOn(t, e)
			require.Equal(t, 1, calls)
			require.Equal(t, "ok", persistenceField(t, e, "aof_last_bgrewrite_status"), "the rewritten file is the log")
			require.Equal(t, "1", persistenceField(t, e, "aof_rewrites"))
			require.True(t, sameFile(t, e.aof.file, path), "appending continues into the file that has the log's name")
			require.Contains(t, logs.String(), "the next sync of it retries the directory first")
			require.Equal(t, filepath.Dir(path), e.unsyncedLogDir)

			runOn(t, e, "SET", "after", "3")
			var err error
			if mode == "retry fails, worker appends" {
				reconfigure(t, e, func(o *Options) { o.AsyncAppend = true })
				_, err = e.FlushAOFAsync(nil)
				e.pollAppend(true)
			} else {
				err = e.FlushAOF()
			}
			if mode == "retry succeeds" {
				require.NoError(t, err)
				require.Equal(t, 2, calls)
				require.Empty(t, e.unsyncedLogDir)
				require.NoError(t, e.CloseAOF())
				restartOn(t, e, path)
				require.Equal(t, "3", runOn(t, e, "GET", "after"))
			} else {
				require.ErrorIs(t, err, syscall.EIO, "a log whose name cannot be made durable stops the server")
				require.Equal(t, "err", persistenceField(t, e, "aof_last_write_status"))
				require.Error(t, e.CloseAOF())
				restartOn(t, e, path)
			}
			require.Equal(t, "1", runOn(t, e, "GET", "before"))
			require.Equal(t, "2", runOn(t, e, "GET", "during"))
		})
	}
}

// A rename can take effect and still report an error, on a network filesystem
// that loses its reply. The rewritten file then has the log's name, and
// appending to the old one would write to a file with no name.
func TestRenameThatTookEffectDespiteItsErrorAdoptsTheNewLog(t *testing.T) {
	// Not parallel: it captures the process's log output, which a test
	// running beside it would write to as well.
	e := newTestEngine(t, Options{})
	logs := captureLog(t)
	reconfigure(t, e, func(o *Options) { o.Fsync = FsyncAlways })
	path := filepath.Join(t.TempDir(), "store.aof")
	require.NoError(t, e.OpenAOF(path))
	runOn(t, e, "SET", "before", "1")
	e.rewriteRename = func(oldPath, newPath string) error {
		if err := os.Rename(oldPath, newPath); err != nil {
			return err
		}
		return &os.LinkError{Op: "rename", Old: oldPath, New: newPath, Err: syscall.ETIMEDOUT}
	}
	require.Equal(t, "Background append only file rewriting started", runOn(t, e, "BGREWRITEAOF"))
	driveRewriteOn(t, e)
	require.Equal(t, "ok", persistenceField(t, e, "aof_last_bgrewrite_status"))
	require.True(t, sameFile(t, e.aof.file, path))
	require.Contains(t, logs.String(), "but the rewritten file has the log's name")
	runOn(t, e, "SET", "after", "2")
	require.NoError(t, e.FlushAOF())
	require.NoError(t, e.CloseAOF())
	restartOn(t, e, path)
	require.Equal(t, "1", runOn(t, e, "GET", "before"))
	require.Equal(t, "2", runOn(t, e, "GET", "after"))
}

// Automatic rewrites are retried as Redis retries them (aofRewriteLimited):
// the first two failures in a row at the next check, no sooner than Redis's
// 100 ms cron tick, then after one minute, two, four and so on up to an hour.
// BGREWRITEAOF is not held back, and a rewrite that finishes ends the limit.
func TestAutomaticRewriteBacksOffAsRedisDoes(t *testing.T) {
	// Not parallel: it captures the process's log output, which a test
	// running beside it would write to as well.
	e := newTestEngine(t, Options{})
	logs := captureLog(t)
	reconfigure(t, e, func(o *Options) { o.Fsync, o.AutoRewritePercentage, o.AutoRewriteMinSize = FsyncAlways, 1, 1 })
	path := filepath.Join(t.TempDir(), "store.aof")
	require.NoError(t, e.OpenAOF(path))
	writes := 0
	e.rewriteFileWrite = func(*os.File, []byte) (int, error) { return 0, syscall.ENOSPC }
	starts := func() int { return strings.Count(logs.String(), "Starting automatic rewriting of AOF on") }
	write := func() {
		t.Helper()
		writes++
		require.Equal(t, "OK", runOn(t, e, "SET", "k"+strings.Repeat("x", writes%7), strings.Repeat("v", writes)))
		require.NoError(t, e.FlushAOF())
	}
	failOne := func() {
		t.Helper()
		before := starts()
		write() // starts it
		require.True(t, e.RewriteActive())
		require.Equal(t, before+1, starts())
		driveRewriteOn(t, e)
	}

	failOne()
	require.Equal(t, "1", persistenceField(t, e, "aof_rewrites_consecutive_failures"))
	write()
	require.False(t, e.RewriteActive(), "no attempt before the next 100 ms tick")
	time.Sleep(rewriteRetryTick + 10*time.Millisecond)
	failOne()
	e.nextAutoRewrite = time.Time{} // the tick, without sleeping through it again
	failOne()
	require.Equal(t, "3", persistenceField(t, e, "aof_rewrites_consecutive_failures"))
	e.nextAutoRewrite = time.Time{}
	for i := 0; i < 20; i++ {
		write()
		require.False(t, e.RewriteActive(), "the third failure in a row holds automatic rewrites back")
	}
	require.Equal(t, 1, strings.Count(logs.String(), "triggered the limit, will retry in 1 minutes"))
	require.Equal(t, 3, starts())

	// Each window that passes allows one attempt, and its failure doubles the
	// next window, up to an hour.
	for _, minutes := range []string{"2", "4", "8", "16", "32", "60", "60"} {
		e.rewriteOutcome.limitedUntil = time.Now().Add(-time.Second)
		e.nextAutoRewrite = time.Time{}
		failOne()
		e.nextAutoRewrite = time.Time{}
		write()
		require.False(t, e.RewriteActive())
		require.Contains(t, logs.String(), "will retry in "+minutes+" minutes")
	}
	require.Equal(t, "10", persistenceField(t, e, "aof_rewrites_consecutive_failures"))

	// BGREWRITEAOF starts at once, limited or not, and its success ends it.
	restoreHooksNow(e)
	require.Equal(t, "Background append only file rewriting started", runOn(t, e, "BGREWRITEAOF"))
	driveRewriteOn(t, e)
	require.Equal(t, "ok", persistenceField(t, e, "aof_last_bgrewrite_status"))
	require.Equal(t, "0", persistenceField(t, e, "aof_rewrites_consecutive_failures"))
	require.False(t, e.rewriteLimited(time.Now()))
	require.Zero(t, e.rewriteOutcome.delay)
	require.Equal(t, "OK", runOn(t, e, "SET", "last", "write"))
	require.NoError(t, e.CloseAOF())
	restartOn(t, e, path)
	require.Equal(t, "write", runOn(t, e, "GET", "last"))
	require.Equal(t, strings.Repeat("v", writes), runOn(t, e, "GET", "k"+strings.Repeat("x", writes%7)))
}

// A budget abort is a failed rewrite too, and the next automatic attempt still
// waits the minute it always has.
func TestBudgetAbortIsAFailedRewriteThatWaitsAMinute(t *testing.T) {
	// Not parallel: it captures the process's log output, which a test
	// running beside it would write to as well.
	e := newTestEngine(t, Options{})
	logs := captureLog(t)
	path := filepath.Join(t.TempDir(), "store.aof")
	require.NoError(t, e.OpenAOF(path))
	runOn(t, e, "SET", "k", "v")
	require.NoError(t, e.StartRewrite())
	e.rewrite.started = time.Now().Add(-31 * time.Second)
	require.NoError(t, e.AdvanceRewrite())
	require.False(t, e.RewriteActive())
	require.Equal(t, "err", persistenceField(t, e, "aof_last_bgrewrite_status"))
	require.Equal(t, "1", persistenceField(t, e, "aof_rewrites_consecutive_failures"))
	require.Equal(t, "1", persistenceField(t, e, "aof_rewrite_budget_aborts"))
	require.Contains(t, logs.String(), "terminated with error: rewrite exceeded its 30-second duration budget")
	require.WithinDuration(t, time.Now().Add(time.Minute), e.nextAutoRewrite, 5*time.Second)
}

// A protocol 2 replica's stream goes on through a failed rewrite, and a
// replica that needs a snapshot while rewrites keep failing is paced by the
// same limit as automatic rewrites, then served by the next one that works.
func TestReplicationV2ThroughFailedRewrites(t *testing.T) {
	// Not parallel: it captures the process's log output, which a test
	// running beside it would write to as well.
	e := newTestEngine(t, Options{})
	setupReplicationV2On(t, e)
	logs := captureLog(t)
	runOn(t, e, "SET", "k", "before")
	frames := snapshotV2On(t, e)
	epoch, base := frames[0].Epoch, frames[len(frames)-1].To

	e.rewriteFileWrite = func(*os.File, []byte) (int, error) { return 0, syscall.ENOSPC }
	require.Equal(t, "Background append only file rewriting started", runOn(t, e, "BGREWRITEAOF"))
	runOn(t, e, "SET", "k", "during")
	driveRewriteOn(t, e)
	require.Equal(t, "err", persistenceField(t, e, "aof_last_bgrewrite_status"))
	runOn(t, e, "SET", "k", "after")
	s := &session{t: t, e: e}
	s.send("MULTI")
	s.send("SET", "a", "1")
	s.send("SET", "b", "1")
	s.send("EXEC")
	require.NoError(t, e.FlushAOF())
	delta := pullV2On(t, e, epoch, base, "", 0)
	require.False(t, delta.Full, "the stream continues from where it was")
	require.Equal(t, epoch, delta.Epoch)
	for _, want := range []string{"during", "after", "MULTI", "EXEC"} {
		require.Contains(t, string(delta.Body), want)
	}

	// Now a replica needs a snapshot while every rewrite fails.
	e.closeReplicationSnapshot()
	attempts := 0
	for i := 0; i < 10; i++ {
		f := pullV2On(t, e, "", 0, "", 0)
		require.True(t, f.Pending)
		if e.RewriteActive() {
			attempts++
			driveRewriteOn(t, e)
		}
	}
	// One failure above already counted, so two more reach the limit.
	require.Equal(t, 2, attempts, "a waiting replica cannot drive a failing disk round a loop")
	require.Contains(t, logs.String(), "triggered the limit, will retry in 1 minutes")
	require.True(t, e.replicationV2.snapshotRequested)

	// The next rewrite that works serves it, whoever started it.
	restoreHooksNow(e)
	require.Equal(t, "Background append only file rewriting started", runOn(t, e, "BGREWRITEAOF"))
	driveRewriteOn(t, e)
	require.Equal(t, "ok", persistenceField(t, e, "aof_last_bgrewrite_status"))
	f := pullV2On(t, e, "", 0, "", 0)
	require.True(t, f.Full)
	require.False(t, f.Pending)
}

// A replica pulls several times a second. A snapshot rewrite that cannot start
// for a lasting reason is refused once, logged once, and not tried again for a
// minute; pulls meanwhile are told to wait.
func TestReplicationV2SnapshotStartFailureIsNotRetriedEveryPull(t *testing.T) {
	// Not parallel: it captures the process's log output, which a test
	// running beside it would write to as well.
	e := newTestEngine(t, Options{})
	setupReplicationV2On(t, e)
	logs := captureLog(t)
	runOn(t, e, "SET", "k", "v")
	require.NoError(t, os.Mkdir(e.aof.path+".rewrite", 0o755))
	reply := runOn(t, e, "KEEL.REPL.PULL2", "", "0", "", "0", strconv.FormatUint(e.failover.term, 10))
	require.Contains(t, reply, "ERR preparing replication snapshot: ")
	for i := 0; i < 5; i++ {
		require.True(t, pullV2On(t, e, "", 0, "", 0).Pending)
		require.False(t, e.RewriteActive())
	}
	require.Equal(t, 1, strings.Count(logs.String(), "Can't rewrite append only file in background: "))
	require.Equal(t, "err", persistenceField(t, e, "aof_last_bgrewrite_status"))

	require.NoError(t, os.Remove(e.aof.path+".rewrite"))
	e.snapshotRetryAt = time.Now().Add(-time.Second) // the minute has passed
	require.True(t, pullV2On(t, e, "", 0, "", 0).Pending)
	require.True(t, e.RewriteActive())
	driveRewriteOn(t, e)
	f := pullV2On(t, e, "", 0, "", 0)
	require.True(t, f.Full)
	require.False(t, f.Pending)
}
