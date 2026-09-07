package core

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/brandopakel/keel/internal/config"
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

var appendPending chan appendResult
var appendBytes int
var appendRetained int
var appendStarted, appendCompleted uint64
var appendWritten, appendSynced uint64
var aofWrite = func(f *os.File, body []byte) (int, error) { return f.Write(body) }

func AppendPending() bool { return appendPending != nil }

func pollAppend(wait bool) {
	if appendPending == nil {
		return
	}
	var result appendResult
	if wait {
		result = <-appendPending
	} else {
		select {
		case result = <-appendPending:
		default:
			return
		}
	}
	appendPending = nil
	appendBytes = 0
	appendRetained = 0
	if result.err == nil {
		appendCompleted = result.end
	}
	recordAOFDigest(result.body[:result.n])
	aof.written += int64(result.n)
	appendWritten += uint64(result.n)
	if result.n > 0 {
		aof.dirty = true
	}
	if result.synced {
		appendSynced = result.end
		aof.dirty = false
		aof.lastSync = time.Now()
	}
	if result.err != nil && aof.failed == nil {
		aof.failed = result.err
	}
}

// FlushAOFAsync starts or polls a batch. ready means its replies may be sent.
// With !ready, only runs covered by AppendAdmission may execute. Expiry,
// replication publication and rewrite transitions must wait for the barrier.
// wake must be safe to call from a worker, including during shutdown.
func FlushAOFAsync(wake func()) (ready bool, err error) {
	pollAppend(false)
	pollAOFSync(false)
	if aof.failed != nil {
		return false, aof.failed
	}
	if appendPending != nil {
		return false, nil
	}
	if aof.file == nil || len(aof.buf) == 0 {
		return true, FlushAOF()
	}
	if len(aof.buf) > maxAsyncAppendBytes {
		aof.failed = fmt.Errorf("async AOF batch exceeds %d bytes", maxAsyncAppendBytes)
		return false, aof.failed
	}
	body := aof.buf
	aof.buf = nil
	file, writeFile, syncFile := aof.file, aofWrite, aofSync
	always := config.AOFFsync == config.FsyncAlways
	result := make(chan appendResult, 1)
	appendPending = result
	appendBytes = len(body)
	appendRetained = cap(body)
	appendStarted += uint64(len(body))
	end := appendStarted
	go func() {
		n, err := timedPersistenceWrite(&appendWriteStats, file, body, writeFile)
		if err == nil && n != len(body) {
			err = io.ErrShortWrite
		}
		synced := false
		if err == nil && always {
			err = timedPersistenceSync(&appendSyncStats, file, syncFile)
			synced = err == nil
		}
		result <- appendResult{n: n, err: err, synced: synced, end: end, body: body}
		if wake != nil {
			wake()
		}
	}()
	return false, nil
}
