package core

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAOFTranscriptLargeValueKeepsBoundedBuffer(t *testing.T) {
	// Not parallel: it reads the process's heap statistics, which a test
	// running beside it would move.
	e := newTestEngine(t, Options{})
	reconfigure(t, e, func(o *Options) { o.Fsync = FsyncNever })
	path := filepath.Join(t.TempDir(), "store.aof")
	require.NoError(t, e.OpenAOF(path))
	t.Cleanup(func() { e.CloseAOF(); e.resetStores() })
	value := strings.Repeat("v", 8<<20)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	require.Equal(t, "OK", runOn(t, e, "SET", "large", value))
	runtime.ReadMemStats(&after)
	t.Logf("large SET allocated %d bytes, retained log capacity %d", after.TotalAlloc-before.TotalAlloc, cap(e.aof.buf))
	require.LessOrEqual(t, cap(e.aof.buf), maxAOFTranscriptBytes)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(6<<20))
	require.NoError(t, e.CloseAOF())
	encoded, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, appendCommand(nil, "SET", "large", value), encoded)
	for i := 0; i < 2; i++ {
		restartOn(t, e, path)
		require.Equal(t, value, runOn(t, e, "GET", "large"))
	}
}

func TestAOFTranscriptAdmitsFourLargeValuesBehindPendingAppend(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	oldWrite := e.aofWrite
	reconfigure(t, e, func(o *Options) { o.MaxMemory, o.MaxKeys = 0, 100000 })
	release, entered := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() {
		once.Do(func() { close(release) })
		e.CloseAOF()
		e.aofWrite = oldWrite
		e.resetStores()
	})
	path := filepath.Join(t.TempDir(), "batch.aof")
	require.NoError(t, e.OpenAOF(path))
	e.aofWrite = func(f *os.File, body []byte) (int, error) {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
		return oldWrite(f, body)
	}
	runOn(t, e, "SET", "first", "safe")
	ready, err := e.FlushAOFAsync(nil)
	require.NoError(t, err)
	require.False(t, ready)
	<-entered
	value := strings.Repeat("v", 1<<20)
	for i := 0; i < 4; i++ {
		key := fmt.Sprint("large:", i)
		commands := []*Command{{Cmd: "SET", Args: []string{key, value, "PX", "60000"}}}
		reserve, _, bounded := e.AppendAdmission(commands)
		require.True(t, bounded)
		require.True(t, e.AppendHasRoom(reserve), "four one-MiB values plus framing should fit")
		before := len(e.aof.buf)
		require.Equal(t, "OK", runOn(t, e, "SET", commands[0].Args...))
		require.LessOrEqual(t, len(e.aof.buf)-before, reserve)
		require.True(t, e.AppendPending(), "commands must not synchronously join the paused worker")
		require.Zero(t, e.AppendReadyOffset(), "pending writes remain unacknowledged")
	}
	require.LessOrEqual(t, cap(e.aof.buf), maxAOFTranscriptBytes)
	once.Do(func() { close(release) })
	require.NoError(t, e.CloseAOF())
	for i := 0; i < 2; i++ {
		restartOn(t, e, path)
		for key := 0; key < 4; key++ {
			require.Equal(t, value, runOn(t, e, "GET", fmt.Sprint("large:", key)))
		}
	}
}

func TestAOFTranscriptDrainsDoNotSyncOrAdvanceRewrite(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	oldSync := e.aofSync
	reconfigure(t, e, func(o *Options) { o.Fsync = FsyncAlways })
	t.Cleanup(func() { e.aofSync = oldSync; e.CloseAOF(); e.resetStores() })
	require.NoError(t, e.OpenAOF(filepath.Join(t.TempDir(), "store.aof")))
	runOn(t, e, "SET", "before", "v")
	require.NoError(t, e.FlushAOF())
	require.NoError(t, e.StartRewrite())
	syncs := 0
	e.aofSync = func(f *os.File) error { syncs++; return oldSync(f) }
	priorReady, priorRewrite := e.AppendReadyOffset(), e.rewrite.written
	runOn(t, e, "SET", "large", strings.Repeat("v", 3*maxAOFTranscriptBytes))
	require.Zero(t, syncs, "fragments cannot fsync a partial command")
	require.Equal(t, priorReady, e.AppendReadyOffset(), "partial command cannot be acknowledged")
	require.Equal(t, priorRewrite, e.rewrite.written, "fragment drains cannot advance or replace the rewrite")
	require.Greater(t, e.appendWritten, priorReady)
	require.True(t, e.RewriteActive())
	require.NoError(t, e.FlushAOF())
	require.Equal(t, 1, syncs)
	require.Equal(t, e.AppendOffset(), e.AppendReadyOffset())
	require.Equal(t, e.AppendOffset(), e.appendSynced)
}

func TestAOFTranscriptPartialWriteNeverAdvancesReplyPrefix(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	oldWrite := e.aofWrite
	t.Cleanup(func() { e.aofWrite = oldWrite; e.CloseAOF(); e.resetStores() })
	path := filepath.Join(t.TempDir(), "store.aof")
	require.NoError(t, e.OpenAOF(path))
	runOn(t, e, "SET", "before", "safe")
	require.NoError(t, e.FlushAOF())
	ready := e.AppendReadyOffset()
	e.aofWrite = func(f *os.File, body []byte) (int, error) { return f.Write(body[:len(body)/2]) }
	runOn(t, e, "SET", "torn", strings.Repeat("v", 3*maxAOFTranscriptBytes))
	// Under everysec the server's log retries the write, as Redis does.
	require.True(t, e.LogRetrying())
	require.ErrorIs(t, e.logFailure.write, io.ErrShortWrite)
	require.Equal(t, ready, e.AppendReadyOffset())
	require.ErrorIs(t, e.FlushAOF(), io.ErrShortWrite)
	require.ErrorIs(t, e.CloseAOF(), io.ErrShortWrite)
	e.aofWrite = oldWrite
	e.resetStores()
	// The short write began at a record boundary, so it was cut back off the
	// log, as Redis truncates one (aof.c 1515-1526): the log is whole, with
	// no torn tail to repair.
	_, err := e.LoadAOF(path)
	require.NoError(t, err)
	for i := 0; i < 2; i++ {
		restartOn(t, e, path)
		require.Equal(t, "safe", runOn(t, e, "GET", "before"))
		require.EqualValues(t, 0, runOn(t, e, "EXISTS", "torn"))
	}
}

func TestAOFTranscriptFailedDrainDoesNotPublishReplication(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	for _, tail := range []int{1, 3 << 20} {
		t.Run(fmt.Sprint(tail), func(t *testing.T) {
			setupReplicationV2On(t, e)
			oldWrite := e.aofWrite
			t.Cleanup(func() { e.aofWrite = oldWrite })
			runOn(t, e, "SET", "acknowledged", "safe")
			require.NoError(t, e.FlushAOF())
			ready := e.AppendReadyOffset()
			before := e.replicationV2.end
			runOn(t, e, "SET", "prior", strings.Repeat("p", 2<<20))
			published := e.replicationV2.end
			require.Greater(t, published, before, "the successful control must publish its command")
			require.Greater(t, len(e.aof.buf), 1)
			e.aofWrite = func(f *os.File, body []byte) (int, error) {
				return f.Write(body[:len(body)-tail])
			}
			torn := strings.Repeat("v", 2*maxAOFTranscriptBytes)
			require.NotPanics(t, func() {
				runOn(t, e, "SET", "torn", torn)
			})
			// The log retries the write, as Redis's does under no, and keeps
			// the rest of the record for it, so the command succeeded and is
			// published whole, as Redis propagates it - never a suffix of it
			// resliced by the failed drain.
			require.True(t, e.LogRetrying())
			require.ErrorIs(t, e.logFailure.write, io.ErrShortWrite)
			require.Equal(t, published+uint64(len(logRecord("SET", "torn", torn))), e.replicationV2.end,
				"the command is published whole, not as a resliced suffix")
			require.Equal(t, ready, e.AppendReadyOffset(), "failure cannot acknowledge either buffered command")
			require.ErrorIs(t, e.FlushAOF(), io.ErrShortWrite)
		})
	}
}

func TestAOFTranscriptReplicationPreservesChunkedPrefixes(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	setupReplicationV2On(t, e)
	snapshot := snapshotV2On(t, e)
	epoch, offset := snapshot[0].Epoch, snapshot[0].To
	value := strings.Repeat("v", 2<<20)
	runOn(t, e, "SET", "prefix", "one")                // must not be published again on the first drain
	runOn(t, e, "SET", "large", value, "PX", "600000") // two staged records
	runOn(t, e, "INCR", "counter")
	runOn(t, e, "SADD", "set", value, "small")
	runOn(t, e, "SPOP", "set", "2") // canonical removal crosses the drain
	runOn(t, e, "INCR", "counter")
	runOn(t, e, "CMS.INITBYDIM", "cms", "65536", "8") // opaque image exceeds a chunk
	runOn(t, e, "CMS.INCRBY", "cms", "hits", "7")
	cms, ok := e.dumpKey("cms")
	require.True(t, ok)
	wantTTL, ok := e.dictStore.GetExpiry("large")
	require.True(t, ok)
	var deltas []ReplicationFrame
	for {
		frame := pullV2On(t, e, epoch, offset, "", 0)
		require.False(t, frame.Full)
		require.False(t, frame.Pending)
		deltas = append(deltas, frame)
		offset = frame.To
		if frame.CaughtUp {
			break
		}
	}
	require.Greater(t, len(deltas), 20)
	path := becomeReplicaV2On(t, e)
	for _, frame := range append(snapshot, deltas...) {
		require.NoError(t, e.ApplyReplication(frame))
	}
	require.True(t, e.replicaReady)
	require.Equal(t, "2", runOn(t, e, "GET", "counter"), "stream drains must not duplicate previous commands")
	require.Equal(t, value, runOn(t, e, "GET", "large"))
	gotTTL, ok := e.dictStore.GetExpiry("large")
	require.True(t, ok)
	require.Equal(t, wantTTL, gotTTL)
	require.EqualValues(t, 0, runOn(t, e, "SCARD", "set"))
	gotCMS, ok := e.dumpKey("cms")
	require.True(t, ok)
	require.Equal(t, cms, gotCMS, "opaque updates preserve the exact image")
	assertCheckpointDigestOn(t, e, path)
}

func TestAOFTranscriptExpiryAndRecreationKeepCanonicalOrder(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	setupReplicationV2On(t, e)
	reconfigure(t, e, func(o *Options) { o.ActiveExpireSamples, o.ActiveExpireRounds = 64, 4 })
	keys := make([]string, 40)
	for i := range keys {
		keys[i] = fmt.Sprint(i) + strings.Repeat("e", 128<<10)
		runOn(t, e, "SET", keys[i], "old")
		e.dictStore.SetExpiryAt(keys[i], 1)
	}
	// These direct expiries model a clock advance after persistence without a
	// sleep. Snapshot expiry semantics are covered by the full replication suite.
	runOn(t, e, "SET", keys[0], "new")
	for e.KeysWithExpiry() > 0 {
		require.Positive(t, e.ExpireCycle())
	}
	require.Equal(t, "new", runOn(t, e, "GET", keys[0]))
	require.EqualValues(t, 1, runOn(t, e, "DBSIZE"))
	require.NoError(t, e.FlushAOF())
	path := e.aof.path
	require.NoError(t, e.CloseAOF())
	for i := 0; i < 2; i++ {
		restartOn(t, e, path)
		require.EqualValues(t, 1, runOn(t, e, "DBSIZE"))
		require.Equal(t, "new", runOn(t, e, "GET", keys[0]))
	}
	var body []byte
	for _, piece := range e.replicationV2.history {
		body = append(body, piece.body...)
	}
	file, err := os.ReadFile(path)
	require.NoError(t, err)
	require.True(t, bytes.Equal(file, body), "expiry drains publish each canonical byte exactly once")
}

func TestAOFTranscriptJoinsOlderAppendBeforeDirectDrain(t *testing.T) {
	// Not parallel: it finds its own drain waiting in a dump of every
	// goroutine's stack, where a test beside it could be waiting in the same
	// functions.
	e := newTestEngine(t, Options{})
	oldWrite := e.aofWrite
	release, entered := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() {
		once.Do(func() { close(release) })
		e.CloseAOF()
		e.aofWrite = oldWrite
		e.resetStores()
	})
	path := filepath.Join(t.TempDir(), "store.aof")
	require.NoError(t, e.OpenAOF(path))
	var calls atomic.Int32
	var firstDone atomic.Bool
	e.aofWrite = func(f *os.File, body []byte) (int, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
			n, err := oldWrite(f, body)
			firstDone.Store(true)
			return n, err
		}
		if !firstDone.Load() {
			return 0, errors.New("direct write overtook pending append")
		}
		return oldWrite(f, body)
	}
	runOn(t, e, "SET", "first", "safe")
	ready, err := e.FlushAOFAsync(nil)
	require.NoError(t, err)
	require.False(t, ready)
	<-entered
	require.False(t, e.AppendHasRoom(2*maxAOFTranscriptBytes), "normal server admission must use its barrier")
	value := strings.Repeat("x", 2*maxAOFTranscriptBytes)
	// Release only after observing the direct drain waiting in pollAppend.
	// A timer alone could release before execution and silently skip the join.
	joined := make(chan bool, 1)
	go func() {
		deadline := time.NewTimer(5 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		stack := make([]byte, 128<<10)
		for {
			n := runtime.Stack(stack, true)
			if n == len(stack) {
				// The dump is every goroutine's, the parallel tests waiting
				// for the serial ones to finish among them, and a full buffer
				// may have cut it off before the frame looked for.
				stack = make([]byte, 2*len(stack))
				continue
			}
			for _, frame := range bytes.Split(stack[:n], []byte("\n\n")) {
				if bytes.Contains(frame, []byte(".pollAppend(")) && bytes.Contains(frame, []byte(".writeAOFBuffer(")) {
					once.Do(func() { close(release) })
					joined <- true
					return
				}
			}
			select {
			case <-tick.C:
			case <-deadline.C:
				once.Do(func() { close(release) })
				joined <- false
				return
			}
		}
	}()
	runOn(t, e, "SET", "second", value) // even a direct core caller must preserve order
	require.True(t, <-joined, "direct drain did not exercise the blocked append join")
	require.NoError(t, e.aof.failed)
	require.NoError(t, e.CloseAOF())
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	want := appendCommand(nil, "SET", "first", "safe")
	want = appendCommand(want, "SET", "second", value)
	require.True(t, bytes.Equal(want, got), "older append is the exact first prefix")
}

func TestAOFTranscriptMassEvictionKeepsBoundedBuffer(t *testing.T) {
	// Not parallel: it reads the process's heap statistics, which a test
	// running beside it would move.
	e := newTestEngine(t, Options{})
	reconfigure(t, e, func(o *Options) { o.Fsync, o.MaxKeys, o.MaxMemory = FsyncNever, 1000, 0 })
	t.Cleanup(func() {
		e.CloseAOF()
		e.resetStores()
	})
	e.resetStores()
	path := filepath.Join(t.TempDir(), "eviction.aof")
	require.NoError(t, e.OpenAOF(path))
	keys := make([]string, 0, 65)
	for i := 0; i < 64; i++ {
		key := fmt.Sprint(i) + strings.Repeat("k", 128<<10)
		keys = append(keys, key)
		require.Equal(t, "OK", runOn(t, e, "SET", key, "v"))
		require.NoError(t, e.FlushAOF())
	}
	keys = append(keys, "last")
	reconfigure(t, e, func(o *Options) { o.MaxKeys = 1 })
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	require.Equal(t, "OK", runOn(t, e, "SET", "last", "v"))
	runtime.ReadMemStats(&after)
	t.Logf("mass eviction allocated %d bytes, retained log capacity %d", after.TotalAlloc-before.TotalAlloc, cap(e.aof.buf))
	require.LessOrEqual(t, cap(e.aof.buf), maxAOFTranscriptBytes)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(8<<20))
	var survivors []string
	for _, key := range keys {
		if e.dictStore.Has(key) {
			survivors = append(survivors, key)
		}
	}
	require.Len(t, survivors, 1)
	require.NoError(t, e.CloseAOF())
	reconfigure(t, e, func(o *Options) { o.MaxKeys = 1000 })
	for i := 0; i < 2; i++ {
		restartOn(t, e, path)
		require.EqualValues(t, 1, runOn(t, e, "DBSIZE"))
		require.Equal(t, "v", runOn(t, e, "GET", survivors[0]))
	}
}
