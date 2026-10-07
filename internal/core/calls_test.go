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

// TestAFailedWriteLatches: once the log's write fails, the call waiting for
// it, and every call after it, read or write, returns ErrPersistence without
// running; Close returns the failure too, and still lets go of the log, which
// reopens on what reached it.
func TestAFailedWriteLatches(t *testing.T) {
	t.Parallel()
	o := Options{AppendOnly: true, AppendFilename: filepath.Join(t.TempDir(), "keel.aof"), Fsync: FsyncAlways}
	e := openTestEngine(t, o)
	mustDo(t, e, "SET", "before", "v")
	disk := errors.New("no space left on the test's disk")
	holding(e, func() {
		e.aofWrite = func(*os.File, []byte) (int, error) { return 0, disk }
	})

	_, err := do(context.Background(), e, "SET", "lost", "v")
	require.ErrorIs(t, err, ErrPersistence)
	require.ErrorIs(t, err, disk)
	assert.EqualError(t, err, "MISCONF Errors writing to the AOF file: no space left on the test's disk")
	for _, cmd := range [][]string{{"GET", "before"}, {"SET", "after", "v"}, {"PING"}} {
		_, err := do(context.Background(), e, cmd[0], cmd[1:]...)
		require.ErrorIs(t, err, ErrPersistence, "%v", cmd)
	}
	holding(e, func() { assert.Equal(t, "$-1\r\n", string(rawReplyOn(t, e, "GET", "after")), "refused calls do not run") })

	err = e.Close()
	require.ErrorIs(t, err, ErrPersistence)
	require.ErrorIs(t, err, disk)
	again := openTestEngine(t, o)
	assert.Equal(t, "$1\r\nv\r\n", mustDo(t, again, "GET", "before"))
	assert.Equal(t, "$-1\r\n", mustDo(t, again, "GET", "lost"), "the failed write never reached the log")
}

// TestAFailedEverysecSyncLatches: under everysec a call returns once its
// record is written, so one that returned before the sync failed has
// succeeded; the failed sync latches, and every later call is refused.
func TestAFailedEverysecSyncLatches(t *testing.T) {
	t.Parallel()
	e := openTestEngine(t, Options{AppendOnly: true, AppendFilename: filepath.Join(t.TempDir(), "keel.aof"), Fsync: FsyncEverySec})
	disk := errors.New("the test's disk lost its cache")
	holding(e, func() { e.aofSync = func(*os.File) error { return disk } })
	mustDo(t, e, "SET", "written", "v")
	eventually(t, e, "the failed sync latched", func() bool { return e.persistenceFailure() != nil })
	_, err := do(context.Background(), e, "GET", "written")
	require.ErrorIs(t, err, ErrPersistence)
	require.ErrorIs(t, err, disk)
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
