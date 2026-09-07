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

// Existing slice-count tests count emitted slices, not idle sync polls. Wait
// only for worker completion here; AdvanceRewrite still consumes the result.
func waitForRewriteSync(t *testing.T) {
	t.Helper()
	if pendingRewriteSync != nil {
		select {
		case <-pendingRewriteSync.done:
		case <-time.After(3 * time.Second):
			t.Fatal("rewrite sync did not finish")
		}
	}
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
	SetRewriteWaker(func() { woken <- struct{}{} })
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
	require.NoError(t, AdvanceRewrite())
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("sync did not start")
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
	require.Empty(t, woken, "only the background preflush needs a wakeup")
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
	require.NoError(t, AdvanceRewrite())
	require.NotNil(t, pendingRewriteSync)
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
			require.NoError(t, AdvanceRewrite())
			job := pendingRewriteSync
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
			require.Same(t, job, pendingRewriteSync)
			require.ErrorContains(t, StartRewrite(), "still releasing")
			require.Equal(t, "OK", run(t, "SET", "after", "survives"))
			require.NoError(t, FlushAOF())
			close(release)
			released = true
			require.NoError(t, CloseAOF()) // shutdown joins the abandoned worker
			require.Nil(t, pendingRewriteSync)
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
	require.NoError(t, AdvanceRewrite())
	waitForRewriteSync(t)
	// The completed preflush covers only the original snapshot.
	require.Equal(t, "OK", run(t, "SET", "during", "survives"))
	var err error
	for i := 0; RewriteActive() && err == nil && i < 100; i++ {
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
