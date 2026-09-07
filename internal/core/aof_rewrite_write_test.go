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
	ResetStores()
	path := filepath.Join(t.TempDir(), "write.aof")
	require.NoError(t, OpenAOF(path))
	oldWrite, oldWake := rewriteFileWrite, rewriteWake
	release, started, wake := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
	released := false
	rewriteFileWrite = func(f *os.File, body []byte) (int, error) {
		select {
		case <-started:
		default:
			close(started)
			<-release
		}
		return f.Write(body)
	}
	SetRewriteWaker(func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	})
	t.Cleanup(func() {
		if !released {
			close(release)
		}
		require.NoError(t, CloseAOF())
		rewriteFileWrite, rewriteWake = oldWrite, oldWake
		ResetStores()
	})
	value := strings.Repeat("before", 1<<18)
	require.Equal(t, "OK", run(t, "SET", "large", value))
	require.NoError(t, FlushAOF())
	require.NoError(t, StartRewrite())
	require.NoError(t, AdvanceRewrite())
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("write did not start")
	}
	require.False(t, RewriteNeedsCycle())
	job := pendingRewriteIO
	require.NotNil(t, job)
	require.LessOrEqual(t, cap(job.body), 2*rewriteRecordSlice)
	for i := 0; i < 100; i++ {
		require.Equal(t, "OK", run(t, "SET", "large", "after"))
		require.Equal(t, "after", run(t, "GET", "large"))
		require.NoError(t, FlushAOF())
		require.Same(t, job, pendingRewriteIO, "blocked writer cannot accumulate queued slices")
	}
	require.Contains(t, rewrite.dirty, "large")
	close(release)
	released = true
	select {
	case <-wake:
	case <-time.After(time.Second):
		t.Fatal("write completion did not wake owner")
	}
	for i := 0; RewriteActive() && i < 1000; i++ {
		waitForRewriteSync(t)
		require.NoError(t, AdvanceRewrite())
	}
	require.False(t, RewriteActive())
	require.NoError(t, CloseAOF())
	for i := 0; i < 2; i++ {
		ResetStores()
		_, err := LoadAOF(path)
		require.NoError(t, err)
		require.Equal(t, "after", run(t, "GET", "large"))
	}
}

func TestRewriteWriteFailureKeepsOriginalLog(t *testing.T) {
	for _, kind := range []string{"error", "short write"} {
		t.Run(kind, func(t *testing.T) {
			ResetStores()
			path := filepath.Join(t.TempDir(), "write.aof")
			require.NoError(t, OpenAOF(path))
			old := rewriteFileWrite
			diskErr := errors.New("injected replacement write failure")
			rewriteFileWrite = func(f *os.File, body []byte) (int, error) {
				if kind == "error" {
					return 0, diskErr
				}
				return f.Write(body[:3])
			}
			t.Cleanup(func() { require.NoError(t, CloseAOF()); rewriteFileWrite = old; ResetStores() })
			require.Equal(t, "OK", run(t, "SET", "before", "survives"))
			require.NoError(t, FlushAOF())
			want := diskErr
			if kind == "short write" {
				want = io.ErrShortWrite
			}
			require.ErrorIs(t, RewriteAOF(), want)
			require.False(t, RewriteActive())
			require.Nil(t, pendingRewriteIO)
			require.Equal(t, "OK", run(t, "SET", "after", "survives"))
			require.NoError(t, CloseAOF())
			ResetStores()
			_, err := LoadAOF(path)
			require.NoError(t, err)
			for _, key := range []string{"before", "after"} {
				require.Equal(t, "survives", run(t, "GET", key))
			}
		})
	}
}

func TestCancelBlockedRewriteWriteOwnsCleanupUntilCompletion(t *testing.T) {
	ResetStores()
	path := filepath.Join(t.TempDir(), "write.aof")
	require.NoError(t, OpenAOF(path))
	old := rewriteFileWrite
	release := make(chan struct{})
	released := false
	rewriteFileWrite = func(f *os.File, body []byte) (int, error) { <-release; return f.Write(body) }
	t.Cleanup(func() {
		if !released {
			close(release)
		}
		require.NoError(t, CloseAOF())
		rewriteFileWrite = old
		ResetStores()
	})
	require.Equal(t, "OK", run(t, "SET", "k", "value"))
	require.NoError(t, FlushAOF())
	require.NoError(t, StartRewrite())
	require.NoError(t, AdvanceRewrite())
	job := pendingRewriteIO
	require.NotNil(t, job)
	CancelRewrite()
	require.False(t, RewriteActive())
	require.Nil(t, rewrite.file)
	require.ErrorContains(t, StartRewrite(), "still releasing")
	require.Equal(t, "OK", run(t, "SET", "after", "survives"))
	require.NoError(t, FlushAOF())
	close(release)
	released = true
	require.NoError(t, CloseAOF())
	require.Nil(t, pendingRewriteIO)
	_, err := os.Stat(path + ".rewrite")
	require.True(t, os.IsNotExist(err))
	ResetStores()
	_, err = LoadAOF(path)
	require.NoError(t, err)
	require.Equal(t, "survives", run(t, "GET", "after"))
}
