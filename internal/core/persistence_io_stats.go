package core

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// Process-lifetime counters use fixed storage and allow INFO to read while an
// I/O worker is blocked. Independent atomic fields are approximate snapshots;
// they do not promise a transactionally consistent multi-field observation.
type persistenceIOStats struct {
	active                                atomic.Int64
	calls, errors, totalNS, maxNS, lastNS atomic.Uint64
}

var appendWriteStats, appendSyncStats persistenceIOStats
var rewriteWriteStats, rewriteSyncStats persistenceIOStats

func (s *persistenceIOStats) finish(start time.Time, err error) {
	elapsed := uint64(time.Since(start))
	s.lastNS.Store(elapsed)
	s.totalNS.Add(elapsed)
	for previous := s.maxNS.Load(); elapsed > previous; previous = s.maxNS.Load() {
		if s.maxNS.CompareAndSwap(previous, elapsed) {
			break
		}
	}
	if err != nil {
		s.errors.Add(1)
	}
	s.calls.Add(1)
	s.active.Add(-1)
}

func timedPersistenceWrite(s *persistenceIOStats, f *os.File, body []byte, write func(*os.File, []byte) (int, error)) (int, error) {
	s.active.Add(1)
	start := time.Now()
	n, err := write(f, body)
	if err == nil && n != len(body) {
		err = io.ErrShortWrite
	}
	s.finish(start, err)
	return n, err
}

func timedPersistenceSync(s *persistenceIOStats, f *os.File, syncFile func(*os.File) error) error {
	s.active.Add(1)
	start := time.Now()
	err := syncFile(f)
	s.finish(start, err)
	return err
}

func persistenceIOInfo(out *strings.Builder) {
	for _, item := range []struct {
		name  string
		stats *persistenceIOStats
	}{
		{"aof_write", &appendWriteStats}, {"aof_sync", &appendSyncStats},
		{"aof_rewrite_write", &rewriteWriteStats}, {"aof_rewrite_sync", &rewriteSyncStats},
	} {
		s := item.stats
		fmt.Fprintf(out, "%s_inflight:%d\r\n%s_calls:%d\r\n%s_errors:%d\r\n%s_total_usec:%d\r\n%s_max_usec:%d\r\n%s_last_usec:%d\r\n",
			item.name, s.active.Load(), item.name, s.calls.Load(), item.name, s.errors.Load(),
			item.name, s.totalNS.Load()/1000, item.name, s.maxNS.Load()/1000, item.name, s.lastNS.Load()/1000)
	}
}
