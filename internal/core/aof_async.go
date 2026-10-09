package core

import (
	"fmt"
	"io"
	"os"
	"time"
)

// One immutable batch crosses the worker boundary. The event loop owns all
// keyspace/AOF state. The optional concurrent mode admits bounded string runs
// while it is pending and gates their replies by the completed logical prefix.
const maxAsyncAppendBytes = 64 << 20

type appendResult struct {
	n      int
	err    error
	synced bool
	end    uint64
	body   []byte
}

// writeLog and syncLog are the log's I/O, each engine's aofWrite and aofSync
// unless a test replaces them on the engine it is failing, to exercise disk
// failures without relying on hardware.
func writeLog(f *os.File, body []byte) (int, error) { return f.Write(body) }
func syncLog(f *os.File) error                      { return f.Sync() }

// truncateLog cuts f back to size: the log's aofTruncate, unless a test
// replaces it on the engine it is failing.
func truncateLog(f *os.File, size int64) error { return f.Truncate(size) }

// AppendPending reports whether a batch of e's log is out with its append
// worker.
func (e *Engine) AppendPending() bool { return e.appendPending != nil }

func (e *Engine) pollAppend(wait bool) {
	if e.appendPending == nil {
		return
	}
	var result appendResult
	if wait {
		result = <-e.appendPending
	} else {
		select {
		case result = <-e.appendPending:
		default:
			return
		}
	}
	e.appendPending = nil
	e.appendBytes = 0
	e.appendRetained = 0
	if result.err == nil {
		e.appendCompleted = result.end
		// A batch is whole records, so the file ends at a record boundary.
		e.aof.midRecord = false
	} else if result.n > 0 && !e.aof.midRecord && e.cutShortWrite(result.n) {
		// Cut back, as the loop's own short write is (aof_transcript.go),
		// so that the log ends at its last whole record.
		result.n = 0
	}
	e.recordAOFDigest(result.body[:result.n])
	e.aof.written += int64(result.n)
	e.appendWritten += uint64(result.n)
	if result.n > 0 {
		e.aof.dirty = true
	}
	if result.synced {
		e.appendSynced = result.end
		e.aof.dirty = false
		e.aof.lastSync = time.Now()
	}
	if result.err == nil {
		if e.logFailure.write != nil {
			e.writeSolved()
		}
		return
	}
	if result.n > 0 {
		e.aof.midRecord = true
	}
	if !e.retriesLogFailures() {
		if e.aof.failed == nil {
			e.aof.failed = result.err
		}
		return
	}
	// Retried, as Redis retries a failed write under everysec and no
	// (log_failures.go): what the worker did not write goes back to the
	// front of the buffer, ahead of whatever was recorded since, and the
	// offsets handed out for it stand, so the next batch writes it in order
	// and the replies held to it go out once it is written. Not before the
	// loop's next tick: a disk that fails at once would otherwise be tried
	// as fast as the worker returns.
	rest := result.body[result.n:]
	e.aof.buf = append(rest[:len(rest):len(rest)], e.aof.buf...)
	e.aof.commandStart += len(rest)
	e.appendStarted -= uint64(len(rest))
	e.appendRetryAt = time.Now().Add(100 * time.Millisecond)
	e.writeFailed(result.err)
}

// FlushAOFAsync starts or polls a batch. ready means its replies may be sent.
// With !ready, only runs covered by AppendAdmission may execute. Expiry,
// replication publication and rewrite transitions must wait for the barrier.
// wake must be safe to call from a worker, including during shutdown.
func (e *Engine) FlushAOFAsync(wake func()) (ready bool, err error) {
	e.pollAppend(false)
	e.pollAOFSync(false)
	if e.aof.failed != nil {
		return false, e.aof.failed
	}
	if e.appendPending != nil {
		return false, nil
	}
	if e.aof.file == nil || len(e.aof.buf) == 0 {
		return true, e.FlushAOF()
	}
	if e.logFailure.write != nil && time.Now().Before(e.appendRetryAt) {
		return false, nil
	}
	if len(e.aof.buf) > maxAsyncAppendBytes {
		e.aof.failed = fmt.Errorf("async AOF batch exceeds %d bytes", maxAsyncAppendBytes)
		return false, e.aof.failed
	}
	always := e.settings.fsync == FsyncAlways
	if always {
		// As in flushAOF: a rewrite's rename whose directory sync failed is
		// finished before this batch can be acknowledged as synced.
		if err := e.syncPendingLogDir(); err != nil {
			e.aof.failed = err
			return false, err
		}
	}
	body := e.aof.buf
	e.aof.buf = nil
	file, writeFile, syncFile := e.aof.file, e.aofWrite, e.aofSync
	writeStats, syncStats := &e.appendWriteStats, &e.appendSyncStats
	result := make(chan appendResult, 1)
	e.appendPending = result
	e.appendBytes = len(body)
	e.appendRetained = cap(body)
	e.appendStarted += uint64(len(body))
	end := e.appendStarted
	go func() {
		n, err := timedPersistenceWrite(writeStats, file, body, writeFile)
		if err == nil && n != len(body) {
			err = io.ErrShortWrite
		}
		synced := false
		if err == nil && always {
			err = timedPersistenceSync(syncStats, file, syncFile)
			synced = err == nil
		}
		result <- appendResult{n: n, err: err, synced: synced, end: end, body: body}
		if wake != nil {
			wake()
		}
	}()
	return false, nil
}
