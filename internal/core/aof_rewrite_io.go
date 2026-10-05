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

// SetRewriteWaker is called by the event-loop owner. Each job captures the
// callback before starting; it must be safe during shutdown, like append wakeups.
func SetRewriteWaker(wake func()) { defaultEngine.SetRewriteWaker(wake) }

// SetRewriteWaker is the package's SetRewriteWaker on e.
func (e *Engine) SetRewriteWaker(wake func()) { e.rewriteWake = wake }

// RewriteNeedsCycle avoids repeatedly waking a loop that cannot advance while
// the replacement write or sync is blocked. Worker completion provides the next wakeup.
func RewriteNeedsCycle() bool { return defaultEngine.RewriteNeedsCycle() }

// RewriteNeedsCycle is the package's RewriteNeedsCycle on e.
func (e *Engine) RewriteNeedsCycle() bool {
	if !e.rewrite.active {
		return false
	}
	// The final handoff also waits for an everysec sync on the original
	// descriptor. Its completion wakes the loop, just like replacement sync.
	if e.rewriteWalkDone() && !e.rewrite.collectionActive && e.aof.syncPending != nil {
		return len(e.aof.syncPending) > 0
	}
	if e.pendingRewriteIO == nil {
		return true
	}
	select {
	case <-e.pendingRewriteIO.done:
		return true
	default:
		return false
	}
}

func (e *Engine) startRewriteSync() {
	e.startRewriteIO(nil)
}

// A non-nil body transfers one immutable encoded slice to the worker. The
// owner cannot construct another slice, sync, close or reuse the path until
// this job completes. A nil body requests the existing bulk preflush.
func (e *Engine) startRewriteIO(body []byte) {
	if e.pendingRewriteIO != nil {
		panic("overlapping replacement-file jobs")
	}
	job := &rewriteIOJob{file: e.rewrite.file, path: e.rewrite.tmpPath,
		written: e.rewrite.written, done: make(chan struct{}), body: body}
	e.pendingRewriteIO = job
	syncFile := e.rewriteFileSync
	writeFile := e.rewriteFileWrite
	wake := e.rewriteWake
	go func() {
		defer func() {
			if wake != nil {
				wake()
			}
		}()
		if job.body == nil {
			job.err = timedPersistenceSync(&rewriteSyncStats, job.file, syncFile)
		} else {
			job.n, job.err = timedPersistenceWrite(&rewriteWriteStats, job.file, job.body, writeFile)
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

func (e *Engine) pollRewriteIO(wait bool) (ready bool, err error) {
	job := e.pendingRewriteIO
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
	e.pendingRewriteIO = nil
	if !e.rewrite.active {
		return true, nil
	} // cancelled job already cleaned up
	if job.err != nil {
		return true, job.err
	}
	if job.body != nil {
		if e.rewrite.digest != nil {
			e.rewrite.digest.Write(job.body[:job.n])
		}
		e.rewrite.written += int64(job.n)
		return true, nil
	}
	e.rewrite.syncedBytes = job.written
	e.rewrite.preSyncComplete = true
	return true, nil
}
