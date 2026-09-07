package core

import (
	"os"
	"sync"
)

// One job owns one replacement-file Sync. It never reads the keyspace. The
// event loop alone consumes its result and decides whether the snapshot is
// still current; writes during Sync remain in the old AOF and the dirty set.
type rewriteSyncJob struct {
	file                *os.File
	path                string
	written             int64
	done                chan struct{}
	err                 error
	mu                  sync.Mutex
	abandoned, finished bool
}

var pendingRewriteSync *rewriteSyncJob
var rewriteFileSync = func(f *os.File) error { return f.Sync() }
var rewriteWake func()

// SetRewriteWaker is called by the event-loop owner. Each job captures the
// callback before starting; it must be safe during shutdown, like append wakeups.
func SetRewriteWaker(wake func()) { rewriteWake = wake }

// RewriteNeedsCycle avoids repeatedly waking a loop that cannot advance while
// the replacement sync is blocked. Worker completion provides the next wakeup.
func RewriteNeedsCycle() bool {
	if !rewrite.active {
		return false
	}
	if pendingRewriteSync == nil {
		return true
	}
	select {
	case <-pendingRewriteSync.done:
		return true
	default:
		return false
	}
}

func startRewriteSync() {
	job := &rewriteSyncJob{file: rewrite.file, path: rewrite.tmpPath,
		written: rewrite.written, done: make(chan struct{})}
	pendingRewriteSync = job
	syncFile := rewriteFileSync
	wake := rewriteWake
	go func() {
		defer func() {
			if wake != nil {
				wake()
			}
		}()
		job.err = syncFile(job.file)
		job.mu.Lock()
		if job.abandoned {
			job.mu.Unlock()
			// Cancellation transfers cleanup to the worker. A new rewrite is
			// refused until this finishes, so the path cannot name a newer file.
			_ = job.file.Close()
			_ = os.Remove(job.path)
			close(job.done)
			return
		}
		job.finished = true
		close(job.done)
		job.mu.Unlock()
	}()
}

// abandon returns true when the worker takes responsibility for cleanup.
func (job *rewriteSyncJob) abandon() bool {
	job.mu.Lock()
	defer job.mu.Unlock()
	if job.finished {
		return false
	}
	job.abandoned = true
	return true
}

func pollRewriteSync(wait bool) (ready bool, err error) {
	job := pendingRewriteSync
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
	pendingRewriteSync = nil
	if !rewrite.active {
		return true, nil
	} // cancelled job already cleaned up
	if job.err != nil {
		return true, job.err
	}
	rewrite.syncedBytes = job.written
	rewrite.preSyncComplete = true
	return true, nil
}
