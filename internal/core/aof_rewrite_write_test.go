package core

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBlockedRewriteWriteKeepsServingAndReconciles(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := filepath.Join(t.TempDir(), "write.aof")
	require.NoError(t, e.OpenAOF(path))
	oldWrite, oldWake := e.rewriteFileWrite, e.rewriteWake
	release, started, wake := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
	released := false
	e.rewriteFileWrite = func(f *os.File, body []byte) (int, error) {
		select {
		case <-started:
		default:
			close(started)
			<-release
		}
		return f.Write(body)
	}
	e.SetRewriteWaker(func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	})
	t.Cleanup(func() {
		if !released {
			close(release)
		}
		require.NoError(t, e.CloseAOF())
		e.rewriteFileWrite, e.rewriteWake = oldWrite, oldWake
		e.resetStores()
	})
	value := strings.Repeat("before", 1<<18)
	require.Equal(t, "OK", runOn(t, e, "SET", "large", value))
	require.NoError(t, e.FlushAOF())
	require.NoError(t, e.StartRewrite())
	require.NoError(t, e.AdvanceRewrite())
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("write did not start")
	}
	require.False(t, e.RewriteNeedsCycle())
	job := e.pendingRewriteIO
	require.NotNil(t, job)
	require.LessOrEqual(t, cap(job.body), 2*rewriteRecordSlice)
	for i := 0; i < 100; i++ {
		require.Equal(t, "OK", runOn(t, e, "SET", "large", "after"))
		require.Equal(t, "after", runOn(t, e, "GET", "large"))
		require.NoError(t, e.FlushAOF())
		require.Same(t, job, e.pendingRewriteIO, "blocked writer cannot accumulate queued slices")
	}
	require.Contains(t, e.rewrite.dirty, "large")
	close(release)
	released = true
	select {
	case <-wake:
	case <-time.After(time.Second):
		t.Fatal("write completion did not wake owner")
	}
	for i := 0; e.RewriteActive() && i < 1000; i++ {
		waitForRewriteSyncOn(t, e)
		require.NoError(t, e.AdvanceRewrite())
	}
	require.False(t, e.RewriteActive())
	require.NoError(t, e.CloseAOF())
	for i := 0; i < 2; i++ {
		e.resetStores()
		_, err := e.LoadAOF(path)
		require.NoError(t, err)
		require.Equal(t, "after", runOn(t, e, "GET", "large"))
	}
}

func TestRewriteWriteFailureKeepsOriginalLog(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	for _, kind := range []string{"error", "short write"} {
		t.Run(kind, func(t *testing.T) {
			e.resetStores()
			path := filepath.Join(t.TempDir(), "write.aof")
			require.NoError(t, e.OpenAOF(path))
			old := e.rewriteFileWrite
			diskErr := errors.New("injected replacement write failure")
			e.rewriteFileWrite = func(f *os.File, body []byte) (int, error) {
				if kind == "error" {
					return 0, diskErr
				}
				return f.Write(body[:3])
			}
			t.Cleanup(func() { require.NoError(t, e.CloseAOF()); e.rewriteFileWrite = old; e.resetStores() })
			require.Equal(t, "OK", runOn(t, e, "SET", "before", "survives"))
			require.NoError(t, e.FlushAOF())
			want := diskErr
			if kind == "short write" {
				want = io.ErrShortWrite
			}
			require.ErrorIs(t, e.RewriteAOF(), want)
			require.False(t, e.RewriteActive())
			require.Nil(t, e.pendingRewriteIO)
			require.Equal(t, "OK", runOn(t, e, "SET", "after", "survives"))
			require.NoError(t, e.CloseAOF())
			e.resetStores()
			_, err := e.LoadAOF(path)
			require.NoError(t, err)
			for _, key := range []string{"before", "after"} {
				require.Equal(t, "survives", runOn(t, e, "GET", key))
			}
		})
	}
}

func TestCancelBlockedRewriteWriteOwnsCleanupUntilCompletion(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := filepath.Join(t.TempDir(), "write.aof")
	require.NoError(t, e.OpenAOF(path))
	old := e.rewriteFileWrite
	release := make(chan struct{})
	released := false
	e.rewriteFileWrite = func(f *os.File, body []byte) (int, error) { <-release; return f.Write(body) }
	t.Cleanup(func() {
		if !released {
			close(release)
		}
		require.NoError(t, e.CloseAOF())
		e.rewriteFileWrite = old
		e.resetStores()
	})
	require.Equal(t, "OK", runOn(t, e, "SET", "k", "value"))
	require.NoError(t, e.FlushAOF())
	require.NoError(t, e.StartRewrite())
	require.NoError(t, e.AdvanceRewrite())
	job := e.pendingRewriteIO
	require.NotNil(t, job)
	e.CancelRewrite()
	require.False(t, e.RewriteActive())
	require.Nil(t, e.rewrite.file)
	require.ErrorContains(t, e.StartRewrite(), "still releasing")
	require.Equal(t, "OK", runOn(t, e, "SET", "after", "survives"))
	require.NoError(t, e.FlushAOF())
	close(release)
	released = true
	require.NoError(t, e.CloseAOF())
	require.Nil(t, e.pendingRewriteIO)
	_, err := os.Stat(path + ".rewrite")
	require.True(t, os.IsNotExist(err))
	e.resetStores()
	_, err = e.LoadAOF(path)
	require.NoError(t, err)
	require.Equal(t, "survives", runOn(t, e, "GET", "after"))
}
