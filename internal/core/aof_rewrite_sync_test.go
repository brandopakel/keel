package core

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Existing slice-count tests count emitted slices, not idle I/O polls. Wait
// only for e's worker to complete here; AdvanceRewrite still consumes the
// result.
func waitForRewriteSyncOn(t *testing.T, e *Engine) {
	t.Helper()
	if e.pendingRewriteIO != nil {
		select {
		case <-e.pendingRewriteIO.done:
		case <-time.After(3 * time.Second):
			t.Fatal("rewrite I/O did not finish")
		}
	}
}

// Sync-specific tests first drain immutable writes. Their injected Sync may
// block indefinitely, so this helper must never wait on the sync job itself.
func advanceToRewriteSync(t *testing.T, e *Engine) {
	t.Helper()
	for i := 0; i < 1000; i++ {
		if e.pendingRewriteIO != nil && e.pendingRewriteIO.body == nil {
			return
		}
		waitForRewriteSyncOn(t, e)
		require.NoError(t, e.AdvanceRewrite())
	}
	t.Fatal("replacement sync did not start")
}

func TestRewriteSyncAllowsWritesAndResynchronizesDirtyState(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := filepath.Join(t.TempDir(), "store.aof")
	require.NoError(t, e.OpenAOF(path))
	release := make(chan struct{})
	started := make(chan struct{})
	woken := make(chan struct{}, 2)
	oldSync := e.rewriteFileSync
	oldWake := e.rewriteWake
	e.SetRewriteWaker(func() {
		select {
		case woken <- struct{}{}:
		default:
		}
	})
	var calls atomic.Int32
	e.rewriteFileSync = func(f *os.File) error {
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		return f.Sync()
	}
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
		e.CancelRewrite()
		require.NoError(t, e.CloseAOF())
		e.rewriteFileSync = oldSync
		e.SetRewriteWaker(oldWake)
		e.resetStores()
	})
	runOn(t, e, "SET", "k", "before")
	require.NoError(t, e.FlushAOF())
	require.NoError(t, e.StartRewrite())
	advanceToRewriteSync(t, e)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("sync did not start")
	}
	// Discard preceding snapshot-write wakeups before checking this Sync's
	// own completion signal.
	select {
	case <-woken:
	case <-time.After(time.Second):
		t.Fatal("preceding snapshot write did not wake the owner")
	}
	require.False(t, e.RewriteNeedsCycle(), "a blocked worker must not cause a self-wakeup spin")
	// The first sync remains blocked while commands execute and the old AOF
	// accepts their records. None of these calls wait for replacement sync.
	require.Equal(t, "OK", runOn(t, e, "SET", "k", "during"))
	require.Equal(t, "during", runOn(t, e, "GET", "k"))
	require.NoError(t, e.FlushAOF())
	require.True(t, e.RewriteActive())
	require.Contains(t, e.rewrite.dirty, "k")
	oldFile, err := os.Stat(path)
	require.NoError(t, err)
	require.True(t, oldFile.Size() > 0)
	close(release)
	released = true
	select {
	case <-woken:
	case <-time.After(time.Second):
		t.Fatal("worker completion did not wake the event loop")
	}
	require.True(t, e.RewriteNeedsCycle())
	for i := 0; e.RewriteActive() && i < 100; i++ {
		waitForRewriteSyncOn(t, e)
		require.Equal(t, "OK", runOn(t, e, "SET", "continued", "write"))
		require.NoError(t, e.FlushAOF())
	}
	require.False(t, e.RewriteActive())
	require.False(t, e.RewriteNeedsCycle())
	// Snapshot writes and the background preflush each wake the owner.
	require.Equal(t, int32(2), calls.Load(), "one preflush plus a final dirty sync; continuous writes cannot cause endless retries")
	require.NoError(t, e.CloseAOF())
	e.resetStores()
	_, err = e.LoadAOF(path)
	require.NoError(t, err)
	require.Equal(t, "during", runOn(t, e, "GET", "k"))
}

func TestCancelRewriteSyncTransfersCleanupWithoutReusingPath(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := filepath.Join(t.TempDir(), "store.aof")
	require.NoError(t, e.OpenAOF(path))
	release := make(chan struct{})
	oldSync := e.rewriteFileSync
	e.rewriteFileSync = func(f *os.File) error { <-release; return f.Sync() }
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
		require.NoError(t, e.CloseAOF())
		e.rewriteFileSync = oldSync
		e.resetStores()
	})
	runOn(t, e, "SET", "k", "value")
	require.NoError(t, e.FlushAOF())
	require.NoError(t, e.StartRewrite())
	advanceToRewriteSync(t, e)
	require.NotNil(t, e.pendingRewriteIO)
	e.CancelRewrite()
	require.False(t, e.RewriteActive())
	require.Nil(t, e.rewrite.file)
	require.ErrorContains(t, e.StartRewrite(), "still releasing")
	require.Equal(t, "OK", runOn(t, e, "SET", "after", "survives"))
	require.NoError(t, e.FlushAOF())
	close(release)
	released = true
	waitForRewriteSyncOn(t, e)
	_, err := os.Stat(path + ".rewrite")
	require.True(t, os.IsNotExist(err))
	require.NoError(t, e.StartRewrite())
	e.CancelRewrite()
	require.NoError(t, e.CloseAOF())
	e.resetStores()
	_, err = e.LoadAOF(path)
	require.NoError(t, err)
	require.Equal(t, "survives", runOn(t, e, "GET", "after"))
}

func TestRewriteSyncFailureKeepsOriginalLog(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := filepath.Join(t.TempDir(), "store.aof")
	require.NoError(t, e.OpenAOF(path))
	oldSync := e.rewriteFileSync
	diskErr := errors.New("injected replacement sync failure")
	e.rewriteFileSync = func(*os.File) error { return diskErr }
	t.Cleanup(func() { require.NoError(t, e.CloseAOF()); e.rewriteFileSync = oldSync; e.resetStores() })
	runOn(t, e, "SET", "k", "value")
	require.NoError(t, e.FlushAOF())
	require.ErrorIs(t, e.RewriteAOF(), diskErr)
	require.False(t, e.RewriteActive())
	require.Equal(t, "OK", runOn(t, e, "SET", "after", "survives"))
	require.NoError(t, e.CloseAOF())
	e.resetStores()
	_, err := e.LoadAOF(path)
	require.NoError(t, err)
	require.Equal(t, "value", runOn(t, e, "GET", "k"))
	require.Equal(t, "survives", runOn(t, e, "GET", "after"))
}

func TestRewriteBudgetAbortWhileSyncOwnsFile(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	for _, budget := range []string{"duration", "dirty bytes"} {
		t.Run(budget, func(t *testing.T) {
			e.resetStores()
			path := filepath.Join(t.TempDir(), "store.aof")
			require.NoError(t, e.OpenAOF(path))
			release := make(chan struct{})
			oldSync := e.rewriteFileSync
			e.rewriteFileSync = func(f *os.File) error { <-release; return f.Sync() }
			released := false
			t.Cleanup(func() {
				if !released {
					close(release)
				}
				require.NoError(t, e.CloseAOF())
				e.rewriteFileSync = oldSync
				e.resetStores()
			})
			runOn(t, e, "SET", "original", "value")
			require.NoError(t, e.FlushAOF())
			require.NoError(t, e.StartRewrite())
			advanceToRewriteSync(t, e)
			job := e.pendingRewriteIO
			require.NotNil(t, job)
			before := e.rewriteBudgetAborts
			if budget == "duration" {
				e.rewrite.started = time.Now().Add(-31 * time.Second)
				require.NoError(t, e.AdvanceRewrite())
			} else {
				e.noteRewriteDirty(strings.Repeat("k", rewriteDirtyBytes))
			}
			require.False(t, e.RewriteActive())
			require.Equal(t, before+1, e.rewriteBudgetAborts)
			require.Same(t, job, e.pendingRewriteIO)
			require.ErrorContains(t, e.StartRewrite(), "still releasing")
			require.Equal(t, "OK", runOn(t, e, "SET", "after", "survives"))
			require.NoError(t, e.FlushAOF())
			close(release)
			released = true
			require.NoError(t, e.CloseAOF()) // shutdown joins the abandoned worker
			require.Nil(t, e.pendingRewriteIO)
			_, err := os.Stat(path + ".rewrite")
			require.True(t, os.IsNotExist(err))
			e.resetStores()
			_, err = e.LoadAOF(path)
			require.NoError(t, err)
			require.Equal(t, "value", runOn(t, e, "GET", "original"))
			require.Equal(t, "survives", runOn(t, e, "GET", "after"))
		})
	}
}

func TestRewriteDirtyTailSyncFailureKeepsAllAcknowledgedWrites(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := filepath.Join(t.TempDir(), "store.aof")
	require.NoError(t, e.OpenAOF(path))
	oldSync := e.rewriteFileSync
	diskErr := errors.New("injected dirty tail sync failure")
	var calls atomic.Int32
	e.rewriteFileSync = func(f *os.File) error {
		if calls.Add(1) == 2 {
			return diskErr
		}
		return f.Sync()
	}
	t.Cleanup(func() { require.NoError(t, e.CloseAOF()); e.rewriteFileSync = oldSync; e.resetStores() })
	runOn(t, e, "SET", "original", "value")
	require.NoError(t, e.FlushAOF())
	require.NoError(t, e.StartRewrite())
	advanceToRewriteSync(t, e)
	waitForRewriteSyncOn(t, e)
	// The completed preflush covers only the original snapshot.
	require.Equal(t, "OK", runOn(t, e, "SET", "during", "survives"))
	for i := 0; e.RewriteActive() && i < 100; i++ {
		waitForRewriteSyncOn(t, e)
		// The failure is the rewrite's, which the log survives, so the loop
		// carries on rather than stopping the server.
		require.NoError(t, e.FlushAOF())
	}
	require.False(t, e.RewriteActive())
	require.ErrorIs(t, e.rewriteOutcome.lastErr, diskErr)
	require.Equal(t, int32(2), calls.Load())
	require.Equal(t, "OK", runOn(t, e, "SET", "after", "survives"))
	require.NoError(t, e.CloseAOF())
	e.resetStores()
	_, err := e.LoadAOF(path)
	require.NoError(t, err)
	for _, key := range []string{"during", "after"} {
		require.Equal(t, "survives", runOn(t, e, "GET", key))
	}
	require.Equal(t, "value", runOn(t, e, "GET", "original"))
}

// Original-log fsync can outlive the snapshot walk. Waiting for it must not
// self-wake indefinitely, and its completion must resume rewrite finalization.
func TestRewriteWaitsForOriginalSyncWithoutSpinning(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	oldSync, oldWake := e.aofSync, e.rewriteWake
	reconfigure(t, e, func(o *Options) { o.Fsync = FsyncEverySec })
	release := make(chan struct{})
	woken := make(chan struct{}, 4)
	e.aofSync = func(f *os.File) error { <-release; return f.Sync() }
	e.SetRewriteWaker(func() {
		select {
		case woken <- struct{}{}:
		default:
		}
	})
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
		require.NoError(t, e.CloseAOF())
		e.aofSync, e.rewriteWake = oldSync, oldWake
		e.resetStores()
	})
	path := filepath.Join(t.TempDir(), "original-sync.aof")
	require.NoError(t, e.OpenAOF(path))
	runOn(t, e, "SET", "k", "value")
	e.aof.lastSync = time.Time{}
	require.NoError(t, e.FlushAOF())
	require.NotNil(t, e.aof.syncPending)
	require.NoError(t, e.StartRewrite())
	for i := 0; !e.rewriteWalkDone() && i < 100; i++ {
		require.NoError(t, e.AdvanceRewrite())
	}
	require.True(t, e.rewriteWalkDone())
	require.True(t, e.RewriteActive())
	require.False(t, e.RewriteNeedsCycle(), "original-file sync must not cause idle polling")
	waitForRewriteSyncOn(t, e)
	select {
	case <-woken:
	case <-time.After(time.Second):
		t.Fatal("snapshot write did not wake the owner before original sync was released")
	}
	close(release)
	released = true
	select {
	case <-woken:
	case <-time.After(time.Second):
		t.Fatal("original sync did not wake the event loop")
	}
	require.True(t, e.RewriteNeedsCycle())
	for i := 0; e.RewriteActive() && i < 100; i++ {
		require.NoError(t, e.AdvanceRewrite())
		waitForRewriteSyncOn(t, e)
	}
	require.False(t, e.RewriteActive())
	require.Equal(t, 1, e.aof.rewrites)
	require.NoError(t, e.CloseAOF())
	e.resetStores()
	_, err := e.LoadAOF(path)
	require.NoError(t, err)
	require.Equal(t, "value", runOn(t, e, "GET", "k"))
}
