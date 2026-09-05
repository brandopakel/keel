package core

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/brandopakel/keel/internal/config"
)

// One immutable batch crosses the worker boundary. The event loop owns all
// keyspace/AOF state and stops command execution until this batch completes.
// This is deliberate backpressure, not an acknowledgement on queue admission.
const maxAsyncAppendBytes = 64 << 20

type appendResult struct {
	n      int
	err    error
	synced bool
}

var appendPending chan appendResult
var appendBytes int
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
	aof.written += int64(result.n)
	if result.n > 0 {
		aof.dirty = true
	}
	if result.synced {
		aof.dirty = false
		aof.lastSync = time.Now()
	}
	if result.err != nil && aof.failed == nil {
		aof.failed = result.err
	}
}

// FlushAOFAsync starts or polls a batch. ready means its replies may be sent.
// The caller must not execute commands, expiry, or rewrites while !ready.
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
	go func() {
		n, err := writeFile(file, body)
		if err == nil && n != len(body) {
			err = io.ErrShortWrite
		}
		synced := false
		if err == nil && always {
			err = syncFile(file)
			synced = err == nil
		}
		result <- appendResult{n: n, err: err, synced: synced}
		if wake != nil {
			wake()
		}
	}()
	return false, nil
}
