package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brandopakel/keel/internal/testlock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// do is a call on e, as an embedded caller makes it, and its reply.
func do(ctx context.Context, e *Engine, name string, args ...string) (string, error) {
	var reply bytes.Buffer
	err := e.Do(ctx, &Command{Cmd: name, Args: args}, &reply)
	return reply.String(), err
}

// mustDo is do, failing t on an error.
func mustDo(t testing.TB, e *Engine, name string, args ...string) string {
	t.Helper()
	reply, err := do(context.Background(), e, name, args...)
	require.NoError(t, err, "%s %v", name, args)
	return reply
}

// gatedSync replaces e's log sync with one that counts, and that waits for
// the gate before it syncs, once the gate is closed for it with hold. The
// test holds nothing while the maintenance goroutine syncs: a sync that waits
// does so under e's lock, as a slow disk's would.
type gatedSync struct {
	syncs   atomic.Int64
	holding atomic.Bool
	entered chan struct{}
	gate    chan struct{}
}

func gateSync(e *Engine) *gatedSync {
	g := &gatedSync{entered: make(chan struct{}, 64), gate: make(chan struct{})}
	holding(e, func() {
		sync := e.aofSync
		e.aofSync = func(f *os.File) error {
			if g.holding.Load() {
				g.entered <- struct{}{}
				<-g.gate
			}
			g.syncs.Add(1)
			return sync(f)
		}
	})
	return g
}

// hold makes every sync wait at the gate until release.
func (g *gatedSync) hold()    { g.holding.Store(true) }
func (g *gatedSync) release() { g.holding.Store(false); close(g.gate) }

// returned runs fn on a goroutine of its own and reports when it has.
func returned(fn func()) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	return done
}

// stillRunning fails t if done has closed. It waits a while first: a slow
// machine can only make done later, never sooner, so this is not a timing
// assertion.
func stillRunning(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
		t.Fatalf("%s returned early", what)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestDoRunsACommandAndAnswersIt: a call runs a command in the scope a
// connection's does, and writes its reply; a command this server does not
// have is an error, as EvalAndResponse returns it.
func TestDoRunsACommandAndAnswersIt(t *testing.T) {
	t.Parallel()
	e := openTestEngine(t, Options{})
	assert.Equal(t, "+OK\r\n", mustDo(t, e, "SET", "k", "v"))
	assert.Equal(t, "$1\r\nv\r\n", mustDo(t, e, "GET", "k"))
	assert.Equal(t, ":1\r\n", mustDo(t, e, "INCR", "n"))
	assert.Equal(t, "-WRONGTYPE Operation against a key holding the wrong kind of value\r\n", mustDo(t, e, "LPUSH", "k", "x"))
	_, err := do(context.Background(), e, "NOSUCH")
	require.Error(t, err)
}

// TestDoRefuses: after Close, a call is refused with ErrClosed; on an engine
// nothing drives but its maker, with errNotDriven; and with its context done,
// with the context's error, without running.
func TestDoRefuses(t *testing.T) {
	t.Parallel()
	e := openTestEngine(t, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := do(ctx, e, "SET", "k", "v")
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, "$-1\r\n", mustDo(t, e, "GET", "k"), "a call whose context was done did not run")

	require.NoError(t, e.Close())
	_, err = do(context.Background(), e, "GET", "k")
	require.ErrorIs(t, err, ErrClosed)
	assert.EqualError(t, err, "instance is closed")

	_, err = do(context.Background(), newTestEngine(t, Options{}), "PING")
	require.ErrorIs(t, err, errNotDriven)
}

// TestACallReturnsOnceItsWriteIsPublished: under every fsync policy, when a
// call returns its record is in the file, and under always it has been
// synced: the published offset covers the log's end.
func TestACallReturnsOnceItsWriteIsPublished(t *testing.T) {
	t.Parallel()
	for _, policy := range []FsyncPolicy{FsyncAlways, FsyncEverySec, FsyncNever} {
		t.Run(string(policy), func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "keel.aof")
			e := openTestEngine(t, Options{AppendOnly: true, AppendFilename: path, Fsync: policy})
			g := gateSync(e)
			for i := range 20 {
				mustDo(t, e, "SET", fmt.Sprintf("k%d", i), "v")
				body, err := os.ReadFile(path)
				require.NoError(t, err)
				require.Contains(t, string(body), logRecord("SET", fmt.Sprintf("k%d", i), "v"), "written before the call returned")
				holding(e, func() {
					encoded, written, synced, ready := e.AOFPositions()
					assert.Equal(t, encoded, ready, "published")
					assert.Equal(t, encoded, written)
					if policy == FsyncAlways {
						assert.Equal(t, encoded, synced, "synced, under always")
						assert.Equal(t, int64(i+1), g.syncs.Load())
					}
				})
			}
		})
	}
}

// TestAReadWaitsForTheWriteItSaw: a read that sees a write whose record is
// still buffered does not return before the record is synced, under always,
// so no caller is told what a crash could still lose.
func TestAReadWaitsForTheWriteItSaw(t *testing.T) {
	t.Parallel()
	e := openTestEngine(t, Options{AppendOnly: true, AppendFilename: filepath.Join(t.TempDir(), "keel.aof"), Fsync: FsyncAlways})
	g := gateSync(e)
	g.hold()
	// Buffered by a holder of the lock, as a call is, and not yet flushed.
	holding(e, func() { runOn(t, e, "SET", "k", "unsynced") })

	var reply string
	read := returned(func() { reply = mustDo(t, e, "GET", "k") })
	<-g.entered
	stillRunning(t, read, "a read of a write that is not yet synced")
	g.release()
	<-read
	assert.Equal(t, "$8\r\nunsynced\r\n", reply)
	assert.Equal(t, int64(1), g.syncs.Load())
}

// TestOneSyncCoversTheCallsWaitingForIt: calls that buffer their records
// while the maintenance goroutine syncs are covered by its next sync, one
// for all of them: group commit. Each sync here takes 2 ms, as a disk's
// might, so the calls queue up behind it.
func TestOneSyncCoversTheCallsWaitingForIt(t *testing.T) {
	t.Parallel()
	o := Options{AppendOnly: true, AppendFilename: filepath.Join(t.TempDir(), "keel.aof"), Fsync: FsyncAlways}
	e := openTestEngine(t, o)
	var syncs atomic.Int64
	holding(e, func() {
		sync := e.aofSync
		e.aofSync = func(f *os.File) error {
			syncs.Add(1)
			time.Sleep(2 * time.Millisecond)
			return sync(f)
		}
	})
	const callers, writes = 8, 25
	var wg sync.WaitGroup
	for c := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range writes {
				mustDo(t, e, "SET", fmt.Sprintf("c%d:%d", c, i), "v")
			}
		}()
	}
	wg.Wait()
	// About 2.4 calls a sync on an M-series Mac: each woken call takes the
	// lock again to see what was published, and the next flush takes it
	// after some of them. One a call would be no group commit at all.
	t.Logf("%d calls, each returned once synced, in %d syncs", callers*writes, syncs.Load())
	assert.Less(t, syncs.Load(), int64(callers*writes), "calls that wait together share a sync")
	require.NoError(t, e.Close())

	again := openTestEngine(t, o)
	assert.Equal(t, fmt.Sprintf(":%d\r\n", callers*writes), mustDo(t, again, "DBSIZE"))
}

// failingDisk replaces e's log write and sync with ones that fail while
// failing is set, as a full or broken disk does; a failing write writes half
// of what it is given first, as a short write does.
type failingDisk struct {
	writes, syncs atomic.Bool
	err           error
}

func failDisk(e *Engine) *failingDisk {
	d := &failingDisk{err: errors.New("no space left on the test's disk")}
	holding(e, func() {
		write, sync := e.aofWrite, e.aofSync
		e.aofWrite = func(f *os.File, b []byte) (int, error) {
			if d.writes.Load() {
				n, _ := write(f, b[:len(b)/2])
				return n, d.err
			}
			return write(f, b)
		}
		e.aofSync = func(f *os.File) error {
			if d.syncs.Load() {
				return d.err
			}
			return sync(f)
		}
	})
	return d
}

// TestUnderAlwaysAFailedWriteOrSyncRefusesEveryCall: under always, where
// Redis exits, a failed write or sync latches. The call waiting for it, and
// every call after it, read or write, returns ErrPersistence without running,
// and Close returns it too, still letting go of the log, which reopens on
// what reached it.
func TestUnderAlwaysAFailedWriteOrSyncRefusesEveryCall(t *testing.T) {
	t.Parallel()
	for _, what := range []string{"write", "sync"} {
		t.Run(what, func(t *testing.T) {
			t.Parallel()
			o := Options{AppendOnly: true, AppendFilename: filepath.Join(t.TempDir(), "keel.aof"), Fsync: FsyncAlways}
			e := openTestEngine(t, o)
			disk := failDisk(e)
			mustDo(t, e, "SET", "before", "v")
			if what == "write" {
				disk.writes.Store(true)
			} else {
				disk.syncs.Store(true)
			}
			_, err := do(context.Background(), e, "SET", "lost", "v")
			require.ErrorIs(t, err, ErrPersistence)
			require.ErrorIs(t, err, disk.err)
			assert.EqualError(t, err, "MISCONF Errors writing to the AOF file: no space left on the test's disk")
			for _, cmd := range [][]string{{"GET", "before"}, {"SET", "after", "v"}, {"PING"}} {
				_, err := do(context.Background(), e, cmd[0], cmd[1:]...)
				require.ErrorIs(t, err, ErrPersistence, "%v", cmd)
			}
			holding(e, func() {
				assert.Equal(t, "$-1\r\n", string(rawReplyOn(t, e, "GET", "after")), "refused calls do not run")
			})
			err = e.Close()
			require.ErrorIs(t, err, ErrPersistence)
			again := openTestEngine(t, o)
			assert.Equal(t, "$1\r\nv\r\n", mustDo(t, again, "GET", "before"))
		})
	}
}

// TestUnderEverysecAndNoAFailedWriteIsRetried: under everysec and no, a failed
// write is Redis's, not fatal. The write in flight when it fails keeps its
// reply, as Redis's does, and is readable; while it fails, write commands and
// PING are refused with MISCONF and do not run, reads are served, and INFO
// says err. Once the disk heals, the maintenance goroutine's next write
// succeeds and clears it, writes are accepted again, and every write that was
// answered OK - the one in flight at the failure included - is in the log.
func TestUnderEverysecAndNoAFailedWriteIsRetried(t *testing.T) {
	t.Parallel()
	for _, policy := range []FsyncPolicy{FsyncEverySec, FsyncNever} {
		t.Run(string(policy), func(t *testing.T) {
			t.Parallel()
			o := Options{AppendOnly: true, AppendFilename: filepath.Join(t.TempDir(), "keel.aof"), Fsync: policy}
			e := openTestEngine(t, o)
			disk := failDisk(e)
			mustDo(t, e, "SET", "before", "v")
			disk.writes.Store(true)

			assert.Equal(t, "+OK\r\n", mustDo(t, e, "SET", "inflight", "v"), "the write in flight at the failure keeps its reply")
			holding(e, func() { require.True(t, e.logRetrying(), "its flush failed") })
			assert.Equal(t, "$1\r\nv\r\n", mustDo(t, e, "GET", "inflight"))
			assert.Equal(t, "$1\r\nv\r\n", mustDo(t, e, "GET", "before"), "reads are served")
			for _, cmd := range [][]string{{"SET", "denied", "v"}, {"INCR", "n"}, {"PING"}} {
				_, err := do(context.Background(), e, cmd[0], cmd[1:]...)
				require.ErrorIs(t, err, ErrPersistence, "%v", cmd)
				require.ErrorIs(t, err, disk.err, "%v", cmd)
			}
			holding(e, func() {
				assert.Equal(t, "$-1\r\n", string(rawReplyOn(t, e, "GET", "denied")), "refused writes do not run")
			})
			assert.Contains(t, mustDo(t, e, "INFO", "persistence"), "aof_last_write_status:err")

			disk.writes.Store(false)
			eventually(t, e, "the retried write succeeded", func() bool { return !e.logRetrying() })
			assert.Equal(t, "+OK\r\n", mustDo(t, e, "SET", "after", "v"), "writes are accepted again")
			assert.Contains(t, mustDo(t, e, "INFO", "persistence"), "aof_last_write_status:ok")
			require.NoError(t, e.Close())

			again := openTestEngine(t, o)
			for _, key := range []string{"before", "inflight", "after"} {
				assert.Equal(t, "$1\r\nv\r\n", mustDo(t, again, "GET", key), key)
			}
			assert.Equal(t, "$-1\r\n", mustDo(t, again, "GET", "denied"))
		})
	}
}

// TestAFailedEverysecSyncIsRetried: under everysec a call returns once its
// record is written, so one that returned before a background sync failed has
// succeeded. While the sync fails, writes are refused and reads served; the
// next due sync retries the bytes that failed to sync, with no new write to
// prompt it - where Redis queues no new sync until more is written - and its
// success clears the failure.
func TestAFailedEverysecSyncIsRetried(t *testing.T) {
	t.Parallel()
	o := Options{AppendOnly: true, AppendFilename: filepath.Join(t.TempDir(), "keel.aof"), Fsync: FsyncEverySec}
	e := openTestEngine(t, o)
	disk := failDisk(e)
	disk.syncs.Store(true)
	mustDo(t, e, "SET", "written", "v")
	eventually(t, e, "the background sync failed", func() bool { return e.logRetrying() })
	_, err := do(context.Background(), e, "SET", "denied", "v")
	require.ErrorIs(t, err, ErrPersistence)
	require.ErrorIs(t, err, disk.err)
	assert.Equal(t, "$1\r\nv\r\n", mustDo(t, e, "GET", "written"))

	disk.syncs.Store(false)
	eventually(t, e, "a retried sync succeeded", func() bool { return !e.logRetrying() })
	holding(e, func() {
		encoded, _, synced, _ := e.AOFPositions()
		assert.Equal(t, encoded, synced, "the bytes that failed to sync are synced")
	})
	mustDo(t, e, "SET", "after", "v")
	require.NoError(t, e.Close())
	again := openTestEngine(t, o)
	assert.Equal(t, "$1\r\nv\r\n", mustDo(t, again, "GET", "written"))
	assert.Equal(t, "$1\r\nv\r\n", mustDo(t, again, "GET", "after"))
}

// TestAFailedDrainOfALargeRecordIsRetried: a record too large for the buffer
// is drained to the log as it is encoded, so the disk can fail in the middle
// of one. Under everysec and no, as Redis keeps its whole record in aof_buf,
// the rest of the record is kept, past the buffer's bound, and the call that
// wrote it keeps its reply; once the disk heals, the retry writes the rest
// after what reached the file, the buffer's extra room is let go, and the log
// reopens on the whole record. Under always, where Redis exits, the failure
// latches, and the log reopens on what came before the record.
func TestAFailedDrainOfALargeRecordIsRetried(t *testing.T) {
	t.Parallel()
	// About 20 MiB of logs, taken before the engines' directories so that it
	// is released only once those files are gone.
	testlock.HoldDiskHeavy(t)
	// Twice the bound, so that what is kept after the first failed drain is
	// more than the buffer may otherwise hold.
	large := strings.Repeat("x", 2*maxAOFTranscriptBytes+128<<10)
	want := fmt.Sprintf("$%d\r\n%s\r\n", len(large), large)
	for _, policy := range []FsyncPolicy{FsyncEverySec, FsyncNever, FsyncAlways} {
		t.Run(string(policy), func(t *testing.T) {
			t.Parallel()
			o := Options{AppendOnly: true, AppendFilename: filepath.Join(t.TempDir(), "keel.aof"), Fsync: policy}
			e := openTestEngine(t, o)
			disk := failDisk(e)
			mustDo(t, e, "SET", "before", "v")
			disk.writes.Store(true)

			reply, err := do(context.Background(), e, "SET", "large", large)
			if policy == FsyncAlways {
				require.ErrorIs(t, err, ErrPersistence)
				require.ErrorIs(t, e.Close(), ErrPersistence)
				again := openTestEngine(t, o)
				assert.Equal(t, "$1\r\nv\r\n", mustDo(t, again, "GET", "before"))
				assert.Equal(t, "$-1\r\n", mustDo(t, again, "GET", "large"), "the torn record is repaired away")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "+OK\r\n", reply, "the write in flight at the failure keeps its reply")
			holding(e, func() {
				require.True(t, e.writeRetrying(), "its drain failed")
				assert.Greater(t, len(e.aof.buf), maxAOFTranscriptBytes, "the rest of the record is kept, past the bound")
			})
			assert.Equal(t, want, mustDo(t, e, "GET", "large"), "reads are served")
			_, err = do(context.Background(), e, "SET", "denied", "v")
			require.ErrorIs(t, err, ErrPersistence)

			disk.writes.Store(false)
			eventually(t, e, "the retried write succeeded", func() bool { return !e.logRetrying() })
			holding(e, func() {
				assert.LessOrEqual(t, cap(e.aof.buf), maxAOFTranscriptBytes, "the buffer's extra room is let go")
			})
			assert.Equal(t, "+OK\r\n", mustDo(t, e, "SET", "after", "v"))
			require.NoError(t, e.Close())

			again := openTestEngine(t, o)
			assert.Equal(t, want, mustDo(t, again, "GET", "large"), "the whole record is in the log")
			for _, key := range []string{"before", "after"} {
				assert.Equal(t, "$1\r\nv\r\n", mustDo(t, again, "GET", key), key)
			}
			assert.Equal(t, "$-1\r\n", mustDo(t, again, "GET", "denied"))
		})
	}
}

// TestACancelledWaitReturnsButTheWriteStands: a call whose context is done
// while it waits for its write returns the context's error, and the write,
// which ran, is still written.
func TestACancelledWaitReturnsButTheWriteStands(t *testing.T) {
	t.Parallel()
	o := Options{AppendOnly: true, AppendFilename: filepath.Join(t.TempDir(), "keel.aof"), Fsync: FsyncAlways}
	e := openTestEngine(t, o)
	g := gateSync(e)
	g.hold()
	ctx, cancel := context.WithCancel(context.Background())
	var err error
	call := returned(func() { _, err = do(ctx, e, "SET", "k", "v") })
	<-g.entered
	stillRunning(t, call, "a call whose write is not synced")
	cancel()
	<-call
	require.ErrorIs(t, err, context.Canceled)
	g.release()
	assert.Equal(t, "$1\r\nv\r\n", mustDo(t, e, "GET", "k"))
	require.NoError(t, e.Close())
	assert.Equal(t, "$1\r\nv\r\n", mustDo(t, openTestEngine(t, o), "GET", "k"))
}

// TestCloseFinishesTheCallsWaitingOnIt: Close waits for the maintenance
// goroutine's cycle, and a call waiting on that cycle's sync returns nil once
// it is done; a call made once Close has begun is refused.
func TestCloseFinishesTheCallsWaitingOnIt(t *testing.T) {
	t.Parallel()
	o := Options{AppendOnly: true, AppendFilename: filepath.Join(t.TempDir(), "keel.aof"), Fsync: FsyncAlways}
	e := openTestEngine(t, o)
	g := gateSync(e)
	g.hold()
	var callErr, closeErr error
	call := returned(func() { _, callErr = do(context.Background(), e, "SET", "k", "v") })
	<-g.entered
	closed := returned(func() { closeErr = e.Close() })
	stillRunning(t, closed, "Close, while the maintenance goroutine syncs")
	g.release()
	<-call
	<-closed
	require.NoError(t, callErr)
	require.NoError(t, closeErr)
	_, err := do(context.Background(), e, "GET", "k")
	require.ErrorIs(t, err, ErrClosed)
	assert.Equal(t, "$1\r\nv\r\n", mustDo(t, openTestEngine(t, o), "GET", "k"))
}

// TestTwoEnginesUnderContention: two engines, one with a log and one without,
// each called by eight goroutines at once, each goroutine on keys of its own,
// with reads, writes, expiries and counters, while both maintenance
// goroutines run. Under -race nothing is shared that should not be, and the
// logged engine, reopened, holds what each goroutine's model says.
func TestTwoEnginesUnderContention(t *testing.T) {
	t.Parallel()
	o := Options{AppendOnly: true, AppendFilename: filepath.Join(t.TempDir(), "keel.aof"), Fsync: FsyncEverySec}
	logged, memory := openTestEngine(t, o), openTestEngine(t, Options{})
	const callers, rounds = 8, 150
	var wg sync.WaitGroup
	for _, e := range []*Engine{logged, memory} {
		for c := range callers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range rounds {
					mustDo(t, e, "INCR", fmt.Sprintf("c%d:count", c))
					mustDo(t, e, "HSET", fmt.Sprintf("c%d:hash", c), fmt.Sprint(i), "v")
					mustDo(t, e, "SET", fmt.Sprintf("c%d:brief", c), "v", "PX", "1")
					mustDo(t, e, "RPUSH", fmt.Sprintf("c%d:list", c), fmt.Sprint(i))
					if got := mustDo(t, e, "GET", fmt.Sprintf("c%d:count", c)); got != fmt.Sprintf("$%d\r\n%d\r\n", len(fmt.Sprint(i+1)), i+1) {
						t.Errorf("caller %d read %q after %d increments", c, got, i+1)
					}
				}
			}()
		}
	}
	wg.Wait()
	for c := range callers {
		for _, e := range []*Engine{logged, memory} {
			assert.Equal(t, fmt.Sprintf(":%d\r\n", rounds), mustDo(t, e, "HLEN", fmt.Sprintf("c%d:hash", c)))
		}
	}
	require.NoError(t, logged.Close())
	again := openTestEngine(t, o)
	for c := range callers {
		assert.Equal(t, fmt.Sprintf("$%d\r\n%d\r\n", len(fmt.Sprint(rounds)), rounds), mustDo(t, again, "GET", fmt.Sprintf("c%d:count", c)))
		assert.Equal(t, fmt.Sprintf(":%d\r\n", rounds), mustDo(t, again, "LLEN", fmt.Sprintf("c%d:list", c)))
	}
	assert.False(t, strings.Contains(mustDo(t, again, "KEYS", "*brief*"), "brief"), "brief keys expired, and their DELs were logged")
}
