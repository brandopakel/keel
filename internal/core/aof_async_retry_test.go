package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// settleAppend drives e's worker as the loop does, until no batch is out:
// each call starts or polls one.
func settleAppend(t *testing.T, e *Engine) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err := e.FlushAOFAsync(nil)
		require.NoError(t, err)
		if !e.AppendPending() {
			return
		}
		require.True(t, time.Now().Before(deadline), "the worker never returned")
		time.Sleep(time.Millisecond)
	}
}

// TestAFailedWorkerBatchIsRetriedFromTheFrontOfTheBuffer: with appends on the
// server's worker, a batch it fails to write is handled as Redis handles a
// failed write under everysec and no. What the worker wrote of it is cut back
// off the log, as the loop's own short write is; the rest goes back to the
// front of the buffer, ahead of what was recorded since, with the offsets
// handed out for it standing, so that the replies held to it stay held until
// it is written; write commands are refused with MISCONF and reads served
// meanwhile; a retry comes no sooner than the loop's next tick; and once the
// disk heals the next batch writes everything in order, and the reopened log
// is whole. Under always the failure latches, as before.
func TestAFailedWorkerBatchIsRetriedFromTheFrontOfTheBuffer(t *testing.T) {
	t.Parallel()
	for _, policy := range []FsyncPolicy{FsyncEverySec, FsyncNever, FsyncAlways} {
		t.Run(string(policy), func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "keel.aof")
			e := newTestEngine(t, Options{Fsync: policy, AsyncAppend: true})
			require.NoError(t, e.OpenAOF(path))
			disk := failDisk(e)
			assert.Equal(t, "+OK\r\n", string(rawReplyOn(t, e, "SET", "before", "v")))
			settleAppend(t, e)
			whole := logSize(t, nil, path)
			written := e.AppendReadyOffset()
			disk.writes.Store(true)

			assert.Equal(t, "+OK\r\n", string(rawReplyOn(t, e, "SET", "inflight", "v")), "ran before the failure was known; its reply is held to its record")
			end := e.AppendOffset()
			if policy == FsyncAlways {
				_, err := e.FlushAOFAsync(nil)
				require.NoError(t, err, "the batch is out")
				for e.AppendPending() {
					time.Sleep(time.Millisecond)
					_, err = e.FlushAOFAsync(nil)
				}
				require.ErrorIs(t, err, disk.err, "latched: the server stops on it")
				require.ErrorIs(t, e.aof.failed, disk.err)
				assert.False(t, e.LogRetrying())
				return
			}
			settleAppend(t, e)
			require.True(t, e.LogRetrying())
			require.NoError(t, e.aof.failed)
			assert.Equal(t, whole, logSize(t, nil, path), "the worker's short write is cut back off the log")
			assert.Equal(t, end, e.AppendOffset(), "the record is back in the buffer at the offset it was given")
			assert.Equal(t, written, e.AppendReadyOffset(), "and its reply is still held")
			assert.Equal(t, int(end-written), e.AppendBufferedBytes())

			const misconf = "-MISCONF Errors writing to the AOF file: no space left on the test's disk\r\n"
			assert.Equal(t, misconf, string(rawReplyOn(t, e, "SET", "denied", "v")))
			assert.Equal(t, misconf, string(rawReplyOn(t, e, "PING")))
			assert.Equal(t, "$1\r\nv\r\n", string(rawReplyOn(t, e, "GET", "inflight")), "reads are served")
			assert.Contains(t, string(rawReplyOn(t, e, "INFO", "persistence")), "aof_last_write_status:err")
			// Expiry and eviction still record while writes are refused, as
			// Redis's do, behind the record waiting for its retry.
			e.aof.replaying = false
			e.appendAOFCommand("DEL", "reaped")
			laterEnd := e.AppendOffset()

			// No retry before the loop's next tick: the batch is not out.
			_, err := e.FlushAOFAsync(nil)
			require.NoError(t, err)
			assert.False(t, e.AppendPending(), "retried no sooner than the next tick")
			disk.writes.Store(false)
			time.Sleep(120 * time.Millisecond)
			settleAppend(t, e)
			assert.False(t, e.LogRetrying(), "the retry succeeded")
			assert.Equal(t, laterEnd, e.AppendReadyOffset(), "everything buffered, in order, is written and published")
			assert.Equal(t, "+OK\r\n", string(rawReplyOn(t, e, "SET", "after", "v")))
			settleAppend(t, e)
			require.NoError(t, e.CloseAOF())

			body, err := os.ReadFile(path)
			require.NoError(t, err)
			log := string(body)
			for _, record := range []string{logRecord("SET", "before", "v"), logRecord("SET", "inflight", "v"), logRecord("DEL", "reaped"), logRecord("SET", "after", "v")} {
				assert.Equal(t, 1, strings.Count(log, record), "%q once", record)
			}
			assert.Less(t, strings.Index(log, logRecord("SET", "inflight", "v")), strings.Index(log, logRecord("DEL", "reaped")), "in order")
			assert.NotContains(t, log, logRecord("SET", "denied", "v"))
			again, torn := reopenedCopy(t, nil, path)
			assert.Zero(t, torn, "the log is whole")
			for _, key := range []string{"before", "inflight", "after"} {
				assert.Equal(t, "$1\r\nv\r\n", mustDo(t, again, "GET", key), key)
			}
		})
	}
}
