package core

import (
	"io"
	"os"
	"sync"
)

// One job owns one replacement-file write or Sync. It never reads the keyspace. The
// event loop alone consumes its result and decides whether the snapshot is
// still current; writes during Sync remain in the old AOF and the dirty set.
type rewriteIOJob struct {
	file                *os.File
	path                string
	written             int64
	done                chan struct{}
	err                 error
	body                []byte
	n                   int
	mu                  sync.Mutex
	abandoned, finished bool
}

var pendingRewriteIO *rewriteIOJob
var rewriteFileSync = func(f *os.File) error { return f.Sync() }
var rewriteFileWrite = func(f *os.File, body []byte) (int, error) { return f.Write(body) }
var rewriteWake func()

// SetRewriteWaker is called by the event-loop owner. Each job captures the
// callback before starting; it must be safe during shutdown, like append wakeups.
func SetRewriteWaker(wake func()) { rewriteWake = wake }

// RewriteNeedsCycle avoids repeatedly waking a loop that cannot advance while
// the replacement write or sync is blocked. Worker completion provides the next wakeup.
func RewriteNeedsCycle() bool {
	if !rewrite.active {
		return false
	}
	// The final handoff also waits for an everysec sync on the original
	// descriptor. Its completion wakes the loop, just like replacement sync.
	if rewriteWalkDone() && !rewrite.collectionActive && aof.syncPending != nil {
		return len(aof.syncPending) > 0
	}
	if pendingRewriteIO == nil {
		return true
	}
	select {
	case <-pendingRewriteIO.done:
		return true
	default:
		return false
	}
}

func startRewriteSync() {
	startRewriteIO(nil)
}

// A non-nil body transfers one immutable encoded slice to the worker. The
// owner cannot construct another slice, sync, close or reuse the path until
// this job completes. A nil body requests the existing bulk preflush.
func startRewriteIO(body []byte) {
	if pendingRewriteIO != nil {
		panic("overlapping replacement-file jobs")
	}
	job := &rewriteIOJob{file: rewrite.file, path: rewrite.tmpPath,
		written: rewrite.written, done: make(chan struct{}), body: body}
	pendingRewriteIO = job
	syncFile := rewriteFileSync
	writeFile := rewriteFileWrite
	wake := rewriteWake
	go func() {
		defer func() {
			if wake != nil {
				wake()
			}
		}()
		if job.body == nil {
			job.err = syncFile(job.file)
		} else {
			job.n, job.err = writeFile(job.file, job.body)
			if job.err == nil && job.n != len(job.body) {
				job.err = io.ErrShortWrite
			}
		}
		job.mu.Lock()
		if job.abandoned {
			job.mu.Unlock()
			// Cancellation transfers cleanup to the worker. A new rewrite is
			// refused until this finishes, so the path cannot name a newer file.
			if err := job.file.Close(); err != nil {
				aofLog("abandoned rewrite I/O close %s: %v", job.path, err)
			}
			if err := os.Remove(job.path); err != nil && !os.IsNotExist(err) {
				aofLog("abandoned rewrite temp file left behind %s: %v", job.path, err)
			}
			close(job.done)
			return
		}
		job.finished = true
		close(job.done)
		job.mu.Unlock()
	}()
}

// abandon returns true when the worker takes responsibility for cleanup.
func (job *rewriteIOJob) abandon() bool {
	job.mu.Lock()
	defer job.mu.Unlock()
	if job.finished {
		return false
	}
	job.abandoned = true
	return true
}

func pollRewriteIO(wait bool) (ready bool, err error) {
	job := pendingRewriteIO
	if job == nil {
		return true, nil
	}
	if wait {
		<-job.done
	} else {
		select {
		case <-job.done:
		default:
			return false, nil
		}
	}
	pendingRewriteIO = nil
	if !rewrite.active {
		return true, nil
	} // cancelled job already cleaned up
	if job.err != nil {
		return true, job.err
	}
	if job.body != nil {
		if rewrite.digest != nil {
			rewrite.digest.Write(job.body[:job.n])
		}
		rewrite.written += int64(job.n)
		return true, nil
	}
	rewrite.syncedBytes = job.written
	rewrite.preSyncComplete = true
	return true, nil
}
