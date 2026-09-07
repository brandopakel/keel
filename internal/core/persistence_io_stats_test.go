package core

import (
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPersistenceIOStatsExposeBlockedWorkAndCountFailures(t *testing.T) {
	var stats persistenceIOStats
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- timedPersistenceSync(&stats, nil, func(*os.File) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	require.Equal(t, int64(1), stats.active.Load())
	require.Zero(t, stats.calls.Load(), "completed-call counters must not hide an in-flight stall")
	close(release)
	require.NoError(t, <-done)
	require.Zero(t, stats.active.Load())
	require.Equal(t, uint64(1), stats.calls.Load())
	require.Positive(t, stats.lastNS.Load())
	require.Equal(t, stats.lastNS.Load(), stats.totalNS.Load())
	require.Equal(t, stats.lastNS.Load(), stats.maxNS.Load())
	// Exercise slow-call accounting with a synthetic start safely beyond the
	// cutoff; the blocked-work assertion above needs no wall-clock delay.
	var slow persistenceIOStats
	slow.active.Add(1)
	slow.finish(time.Now().Add(-time.Minute), nil)
	require.Equal(t, uint64(1), slow.slowCalls.Load())
	require.Equal(t, slow.lastNS.Load(), slow.slowLastNS.Load())
	require.Positive(t, slow.slowLastUnixUS.Load())
	errDisk := errors.New("storage failure")
	_, err := timedPersistenceWrite(&stats, nil, []byte("abc"), func(*os.File, []byte) (int, error) { return 0, errDisk })
	require.ErrorIs(t, err, errDisk)
	_, err = timedPersistenceWrite(&stats, nil, []byte("abc"), func(*os.File, []byte) (int, error) { return 1, nil })
	require.ErrorIs(t, err, io.ErrShortWrite)
	require.Equal(t, uint64(3), stats.calls.Load())
	require.Equal(t, uint64(2), stats.errors.Load())
	require.GreaterOrEqual(t, stats.totalNS.Load(), stats.maxNS.Load())
}

func TestPersistenceIOStatsSupportConcurrentCompletionAndObservation(t *testing.T) {
	var stats persistenceIOStats
	var workers sync.WaitGroup
	for i := 0; i < 32; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			_ = timedPersistenceSync(&stats, nil, func(*os.File) error { return nil })
		}()
	}
	for i := 0; i < 100; i++ {
		_ = stats.active.Load()
		_ = stats.calls.Load()
		_ = stats.maxNS.Load()
	}
	workers.Wait()
	require.Equal(t, uint64(32), stats.calls.Load())
	require.Zero(t, stats.active.Load())
	require.Zero(t, stats.errors.Load())
	var out strings.Builder
	persistenceIOInfo(&out)
	for _, prefix := range []string{"aof_write", "aof_sync", "aof_rewrite_write", "aof_rewrite_sync", "aof_rewrite_final_sync", "aof_rewrite_finalize"} {
		for _, field := range []string{"inflight", "calls", "errors", "total_usec", "max_usec", "last_usec", "slow_calls", "slow_last_usec", "slow_last_unix_usec"} {
			require.Contains(t, out.String(), prefix+"_"+field+":")
		}
	}
}

func BenchmarkPersistenceIOTimingOverhead(b *testing.B) {
	var stats persistenceIOStats
	syncFile := func(*os.File) error { return nil }
	for _, timed := range []bool{false, true} {
		name := "direct"
		if timed {
			name = "timed"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if timed {
					_ = timedPersistenceSync(&stats, nil, syncFile)
				} else {
					_ = syncFile(nil)
				}
			}
		})
	}
}
