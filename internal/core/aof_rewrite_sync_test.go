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

	"github.com/brandopakel/keel/internal/config"
)

// Existing slice-count tests count emitted slices, not idle I/O polls. Wait
// only for worker completion here; AdvanceRewrite still consumes the result.
func waitForRewriteSync(t *testing.T) {
	t.Helper()
	if pendingRewriteIO != nil {
		select {
		case <-pendingRewriteIO.done:
		case <-time.After(3 * time.Second):
			t.Fatal("rewrite I/O did not finish")
		}
	}
}

// Sync-specific tests first drain immutable writes. Their injected Sync may
// block indefinitely, so this helper must never wait on the sync job itself.
func advanceToRewriteSync(t *testing.T) {
	t.Helper()
	for i := 0; i < 1000; i++ {
		if pendingRewriteIO != nil && pendingRewriteIO.body == nil {
			return
		}
		waitForRewriteSync(t)
		require.NoError(t, AdvanceRewrite())
	}
	t.Fatal("replacement sync did not start")
}

func TestRewriteSyncAllowsWritesAndResynchronizesDirtyState(t *testing.T) {
	ResetStores()
	path := filepath.Join(t.TempDir(), "store.aof")
	require.NoError(t, OpenAOF(path))
	release := make(chan struct{})
	started := make(chan struct{})
	woken := make(chan struct{}, 2)
	oldSync := rewriteFileSync
	oldWake := rewriteWake
	SetRewriteWaker(func() {
		select {
		case woken <- struct{}{}:
		default:
		}
	})
	var calls atomic.Int32
	rewriteFileSync = func(f *os.File) error {
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
		CancelRewrite()
		require.NoError(t, CloseAOF())
		rewriteFileSync = oldSync
		SetRewriteWaker(oldWake)
		ResetStores()
	})
	run(t, "SET", "k", "before")
	require.NoError(t, FlushAOF())
	require.NoError(t, StartRewrite())
	advanceToRewriteSync(t)
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
	require.False(t, RewriteNeedsCycle(), "a blocked worker must not cause a self-wakeup spin")
	// The first sync remains blocked while commands execute and the old AOF
	// accepts their records. None of these calls wait for replacement sync.
	require.Equal(t, "OK", run(t, "SET", "k", "during"))
	require.Equal(t, "during", run(t, "GET", "k"))
	require.NoError(t, FlushAOF())
	require.True(t, RewriteActive())
	require.Contains(t, rewrite.dirty, "k")
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
	require.True(t, RewriteNeedsCycle())
	for i := 0; RewriteActive() && i < 100; i++ {
		waitForRewriteSync(t)
		require.Equal(t, "OK", run(t, "SET", "continued", "write"))
		require.NoError(t, FlushAOF())
	}
	require.False(t, RewriteActive())
	require.False(t, RewriteNeedsCycle())
	// Snapshot writes and the background preflush each wake the owner.
	require.Equal(t, int32(2), calls.Load(), "one preflush plus a final dirty sync; continuous writes cannot cause endless retries")
	require.NoError(t, CloseAOF())
	ResetStores()
	_, err = LoadAOF(path)
	require.NoError(t, err)
	require.Equal(t, "during", run(t, "GET", "k"))
}

func TestCancelRewriteSyncTransfersCleanupWithoutReusingPath(t *testing.T) {
	ResetStores()
	path := filepath.Join(t.TempDir(), "store.aof")
	require.NoError(t, OpenAOF(path))
	release := make(chan struct{})
	oldSync := rewriteFileSync
	rewriteFileSync = func(f *os.File) error { <-release; return f.Sync() }
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
		require.NoError(t, CloseAOF())
		rewriteFileSync = oldSync
		ResetStores()
	})
	run(t, "SET", "k", "value")
	require.NoError(t, FlushAOF())
	require.NoError(t, StartRewrite())
	advanceToRewriteSync(t)
	require.NotNil(t, pendingRewriteIO)
	CancelRewrite()
	require.False(t, RewriteActive())
	require.Nil(t, rewrite.file)
	require.ErrorContains(t, StartRewrite(), "still releasing")
	require.Equal(t, "OK", run(t, "SET", "after", "survives"))
	require.NoError(t, FlushAOF())
	close(release)
	released = true
	waitForRewriteSync(t)
	_, err := os.Stat(path + ".rewrite")
	require.True(t, os.IsNotExist(err))
	require.NoError(t, StartRewrite())
	CancelRewrite()
	require.NoError(t, CloseAOF())
	ResetStores()
	_, err = LoadAOF(path)
	require.NoError(t, err)
	require.Equal(t, "survives", run(t, "GET", "after"))
}

func TestRewriteSyncFailureKeepsOriginalLog(t *testing.T) {
	ResetStores()
	path := filepath.Join(t.TempDir(), "store.aof")
	require.NoError(t, OpenAOF(path))
	oldSync := rewriteFileSync
	diskErr := errors.New("injected replacement sync failure")
	rewriteFileSync = func(*os.File) error { return diskErr }
	t.Cleanup(func() { require.NoError(t, CloseAOF()); rewriteFileSync = oldSync; ResetStores() })
	run(t, "SET", "k", "value")
	require.NoError(t, FlushAOF())
	require.ErrorIs(t, RewriteAOF(), diskErr)
	require.False(t, RewriteActive())
	require.Equal(t, "OK", run(t, "SET", "after", "survives"))
	require.NoError(t, CloseAOF())
	ResetStores()
	_, err := LoadAOF(path)
	require.NoError(t, err)
	require.Equal(t, "value", run(t, "GET", "k"))
	require.Equal(t, "survives", run(t, "GET", "after"))
}

func TestRewriteBudgetAbortWhileSyncOwnsFile(t *testing.T) {
	for _, budget := range []string{"duration", "dirty bytes"} {
		t.Run(budget, func(t *testing.T) {
			ResetStores()
			path := filepath.Join(t.TempDir(), "store.aof")
			require.NoError(t, OpenAOF(path))
			release := make(chan struct{})
			oldSync := rewriteFileSync
			rewriteFileSync = func(f *os.File) error { <-release; return f.Sync() }
			released := false
			t.Cleanup(func() {
				if !released {
					close(release)
				}
				require.NoError(t, CloseAOF())
				rewriteFileSync = oldSync
				ResetStores()
			})
			run(t, "SET", "original", "value")
			require.NoError(t, FlushAOF())
			require.NoError(t, StartRewrite())
			advanceToRewriteSync(t)
			job := pendingRewriteIO
			require.NotNil(t, job)
			before := rewriteBudgetAborts
			if budget == "duration" {
				rewrite.started = time.Now().Add(-31 * time.Second)
				require.NoError(t, AdvanceRewrite())
			} else {
				noteRewriteDirty(strings.Repeat("k", rewriteDirtyBytes))
			}
			require.False(t, RewriteActive())
			require.Equal(t, before+1, rewriteBudgetAborts)
			require.Same(t, job, pendingRewriteIO)
			require.ErrorContains(t, StartRewrite(), "still releasing")
			require.Equal(t, "OK", run(t, "SET", "after", "survives"))
			require.NoError(t, FlushAOF())
			close(release)
			released = true
			require.NoError(t, CloseAOF()) // shutdown joins the abandoned worker
			require.Nil(t, pendingRewriteIO)
			_, err := os.Stat(path + ".rewrite")
			require.True(t, os.IsNotExist(err))
			ResetStores()
			_, err = LoadAOF(path)
			require.NoError(t, err)
			require.Equal(t, "value", run(t, "GET", "original"))
			require.Equal(t, "survives", run(t, "GET", "after"))
		})
	}
}

func TestRewriteDirtyTailSyncFailureKeepsAllAcknowledgedWrites(t *testing.T) {
	ResetStores()
	path := filepath.Join(t.TempDir(), "store.aof")
	require.NoError(t, OpenAOF(path))
	oldSync := rewriteFileSync
	diskErr := errors.New("injected dirty tail sync failure")
	var calls atomic.Int32
	rewriteFileSync = func(f *os.File) error {
		if calls.Add(1) == 2 {
			return diskErr
		}
		return f.Sync()
	}
	t.Cleanup(func() { require.NoError(t, CloseAOF()); rewriteFileSync = oldSync; ResetStores() })
	run(t, "SET", "original", "value")
	require.NoError(t, FlushAOF())
	require.NoError(t, StartRewrite())
	advanceToRewriteSync(t)
	waitForRewriteSync(t)
	// The completed preflush covers only the original snapshot.
	require.Equal(t, "OK", run(t, "SET", "during", "survives"))
	var err error
	for i := 0; RewriteActive() && err == nil && i < 100; i++ {
		waitForRewriteSync(t)
		err = FlushAOF()
	}
	require.ErrorIs(t, err, diskErr)
	require.False(t, RewriteActive())
	require.Equal(t, int32(2), calls.Load())
	require.Equal(t, "OK", run(t, "SET", "after", "survives"))
	require.NoError(t, CloseAOF())
	ResetStores()
	_, err = LoadAOF(path)
	require.NoError(t, err)
	for _, key := range []string{"during", "after"} {
		require.Equal(t, "survives", run(t, "GET", key))
	}
	require.Equal(t, "value", run(t, "GET", "original"))
}

// Original-log fsync can outlive the snapshot walk. Waiting for it must not
// self-wake indefinitely, and its completion must resume rewrite finalization.
func TestRewriteWaitsForOriginalSyncWithoutSpinning(t *testing.T) {
	ResetStores()
	oldPolicy, oldSync, oldWake := config.AOFFsync, aofSync, rewriteWake
	config.AOFFsync = config.FsyncEverySec
	release := make(chan struct{})
	woken := make(chan struct{}, 4)
	aofSync = func(f *os.File) error { <-release; return f.Sync() }
	SetRewriteWaker(func() {
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
		require.NoError(t, CloseAOF())
		config.AOFFsync, aofSync, rewriteWake = oldPolicy, oldSync, oldWake
		ResetStores()
	})
	path := filepath.Join(t.TempDir(), "original-sync.aof")
	require.NoError(t, OpenAOF(path))
	run(t, "SET", "k", "value")
	aof.lastSync = time.Time{}
	require.NoError(t, FlushAOF())
	require.NotNil(t, aof.syncPending)
	require.NoError(t, StartRewrite())
	for i := 0; !rewriteWalkDone() && i < 100; i++ {
		require.NoError(t, AdvanceRewrite())
	}
	require.True(t, rewriteWalkDone())
	require.True(t, RewriteActive())
	require.False(t, RewriteNeedsCycle(), "original-file sync must not cause idle polling")
	waitForRewriteSync(t)
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
	require.True(t, RewriteNeedsCycle())
	for i := 0; RewriteActive() && i < 100; i++ {
		require.NoError(t, AdvanceRewrite())
		waitForRewriteSync(t)
	}
	require.False(t, RewriteActive())
	require.Equal(t, 1, aof.rewrites)
	require.NoError(t, CloseAOF())
	ResetStores()
	_, err := LoadAOF(path)
	require.NoError(t, err)
	require.Equal(t, "value", run(t, "GET", "k"))
}
