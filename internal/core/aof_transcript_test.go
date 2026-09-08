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

	"github.com/brandopakel/keel/internal/config"
	"github.com/stretchr/testify/require"
)

func TestAOFTranscriptLargeValueKeepsBoundedBuffer(t *testing.T) {
	ResetStores()
	oldPolicy := config.AOFFsync
	config.AOFFsync = config.FsyncNever
	path := filepath.Join(t.TempDir(), "store.aof")
	require.NoError(t, OpenAOF(path))
	t.Cleanup(func() { CloseAOF(); config.AOFFsync = oldPolicy; ResetStores() })
	value := strings.Repeat("v", 8<<20)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	require.Equal(t, "OK", run(t, "SET", "large", value))
	runtime.ReadMemStats(&after)
	t.Logf("large SET allocated %d bytes, retained log capacity %d", after.TotalAlloc-before.TotalAlloc, cap(aof.buf))
	require.LessOrEqual(t, cap(aof.buf), maxAOFTranscriptBytes)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(6<<20))
	require.NoError(t, CloseAOF())
	encoded, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, appendCommand(nil, "SET", "large", value), encoded)
	for i := 0; i < 2; i++ {
		restart(t, path)
		require.Equal(t, value, run(t, "GET", "large"))
	}
}

func TestAOFTranscriptAdmitsFourLargeValuesBehindPendingAppend(t *testing.T) {
	ResetStores()
	oldWrite, oldMemory, oldKeys := aofWrite, config.MaxMemory, config.KeyNumberLimit
	config.MaxMemory, config.KeyNumberLimit = 0, 100000
	release, entered := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() {
		once.Do(func() { close(release) })
		CloseAOF()
		aofWrite, config.MaxMemory, config.KeyNumberLimit = oldWrite, oldMemory, oldKeys
		ResetStores()
	})
	path := filepath.Join(t.TempDir(), "batch.aof")
	require.NoError(t, OpenAOF(path))
	aofWrite = func(f *os.File, body []byte) (int, error) {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
		return oldWrite(f, body)
	}
	run(t, "SET", "first", "safe")
	ready, err := FlushAOFAsync(nil)
	require.NoError(t, err)
	require.False(t, ready)
	<-entered
	value := strings.Repeat("v", 1<<20)
	for i := 0; i < 4; i++ {
		key := fmt.Sprint("large:", i)
		commands := []*Command{{Cmd: "SET", Args: []string{key, value, "PX", "60000"}}}
		reserve, _, bounded := AppendAdmission(commands)
		require.True(t, bounded)
		require.True(t, AppendHasRoom(reserve), "four one-MiB values plus framing should fit")
		before := len(aof.buf)
		require.Equal(t, "OK", run(t, "SET", commands[0].Args...))
		require.LessOrEqual(t, len(aof.buf)-before, reserve)
		require.True(t, AppendPending(), "commands must not synchronously join the paused worker")
		require.Zero(t, AppendReadyOffset(), "pending writes remain unacknowledged")
	}
	require.LessOrEqual(t, cap(aof.buf), maxAOFTranscriptBytes)
	once.Do(func() { close(release) })
	require.NoError(t, CloseAOF())
	for i := 0; i < 2; i++ {
		restart(t, path)
		for key := 0; key < 4; key++ {
			require.Equal(t, value, run(t, "GET", fmt.Sprint("large:", key)))
		}
	}
}

func TestAOFTranscriptDrainsDoNotSyncOrAdvanceRewrite(t *testing.T) {
	ResetStores()
	oldSync, oldPolicy := aofSync, config.AOFFsync
	config.AOFFsync = config.FsyncAlways
	t.Cleanup(func() { aofSync = oldSync; CloseAOF(); config.AOFFsync = oldPolicy; ResetStores() })
	require.NoError(t, OpenAOF(filepath.Join(t.TempDir(), "store.aof")))
	run(t, "SET", "before", "v")
	require.NoError(t, FlushAOF())
	require.NoError(t, StartRewrite())
	syncs := 0
	aofSync = func(f *os.File) error { syncs++; return oldSync(f) }
	priorReady, priorRewrite := AppendReadyOffset(), rewrite.written
	run(t, "SET", "large", strings.Repeat("v", 3*maxAOFTranscriptBytes))
	require.Zero(t, syncs, "fragments cannot fsync a partial command")
	require.Equal(t, priorReady, AppendReadyOffset(), "partial command cannot be acknowledged")
	require.Equal(t, priorRewrite, rewrite.written, "fragment drains cannot advance or replace the rewrite")
	require.Greater(t, appendWritten, priorReady)
	require.True(t, RewriteActive())
	require.NoError(t, FlushAOF())
	require.Equal(t, 1, syncs)
	require.Equal(t, AppendOffset(), AppendReadyOffset())
	require.Equal(t, AppendOffset(), appendSynced)
}

func TestAOFTranscriptPartialWriteNeverAdvancesReplyPrefix(t *testing.T) {
	ResetStores()
	oldWrite := aofWrite
	t.Cleanup(func() { aofWrite = oldWrite; CloseAOF(); ResetStores() })
	path := filepath.Join(t.TempDir(), "store.aof")
	require.NoError(t, OpenAOF(path))
	run(t, "SET", "before", "safe")
	require.NoError(t, FlushAOF())
	ready := AppendReadyOffset()
	aofWrite = func(f *os.File, body []byte) (int, error) { return f.Write(body[:len(body)/2]) }
	run(t, "SET", "torn", strings.Repeat("v", 3*maxAOFTranscriptBytes))
	require.ErrorIs(t, aof.failed, io.ErrShortWrite)
	require.Equal(t, ready, AppendReadyOffset())
	require.ErrorIs(t, FlushAOF(), io.ErrShortWrite)
	require.ErrorIs(t, CloseAOF(), io.ErrShortWrite)
	aofWrite = oldWrite
	ResetStores()
	_, err := LoadAOF(path)
	var torn *truncatedAOF
	require.ErrorAs(t, err, &torn)
	require.NoError(t, RepairAOFTail(err))
	for i := 0; i < 2; i++ {
		restart(t, path)
		require.Equal(t, "safe", run(t, "GET", "before"))
		require.EqualValues(t, 0, run(t, "EXISTS", "torn"))
	}
}

func TestAOFTranscriptFailedDrainDoesNotPublishReplication(t *testing.T) {
	for _, tail := range []int{1, 3 << 20} {
		t.Run(fmt.Sprint(tail), func(t *testing.T) {
			setupReplicationV2(t)
			oldWrite := aofWrite
			t.Cleanup(func() { aofWrite = oldWrite })
			run(t, "SET", "acknowledged", "safe")
			require.NoError(t, FlushAOF())
			ready := AppendReadyOffset()
			before := replicationV2.end
			run(t, "SET", "prior", strings.Repeat("p", 2<<20))
			published := replicationV2.end
			require.Greater(t, published, before, "the successful control must publish its command")
			require.Greater(t, len(aof.buf), 1)
			aofWrite = func(f *os.File, body []byte) (int, error) {
				return f.Write(body[:len(body)-tail])
			}
			require.NotPanics(t, func() {
				run(t, "SET", "torn", strings.Repeat("v", 2*maxAOFTranscriptBytes))
			})
			require.ErrorIs(t, aof.failed, io.ErrShortWrite)
			require.Equal(t, published, replicationV2.end, "failed drain must not publish a resliced suffix")
			require.Equal(t, ready, AppendReadyOffset(), "failure cannot acknowledge either buffered command")
			require.ErrorIs(t, FlushAOF(), io.ErrShortWrite)
		})
	}
}

func TestAOFTranscriptReplicationPreservesChunkedPrefixes(t *testing.T) {
	setupReplicationV2(t)
	snapshot := snapshotV2(t)
	epoch, offset := snapshot[0].Epoch, snapshot[0].To
	value := strings.Repeat("v", 2<<20)
	run(t, "SET", "prefix", "one")                // must not be published again on the first drain
	run(t, "SET", "large", value, "PX", "600000") // two staged records
	run(t, "INCR", "counter")
	run(t, "SADD", "set", value, "small")
	run(t, "SPOP", "set", "2") // canonical removal crosses the drain
	run(t, "INCR", "counter")
	run(t, "CMS.INITBYDIM", "cms", "65536", "8") // opaque image exceeds a chunk
	run(t, "CMS.INCRBY", "cms", "hits", "7")
	cms, ok := dumpKey("cms")
	require.True(t, ok)
	wantTTL, ok := dictStore.GetExpiry("large")
	require.True(t, ok)
	var deltas []ReplicationFrame
	for {
		frame := pullV2(t, epoch, offset, "", 0)
		require.False(t, frame.Full)
		require.False(t, frame.Pending)
		deltas = append(deltas, frame)
		offset = frame.To
		if frame.CaughtUp {
			break
		}
	}
	require.Greater(t, len(deltas), 20)
	path := becomeReplicaV2(t)
	for _, frame := range append(snapshot, deltas...) {
		require.NoError(t, ApplyReplication(frame))
	}
	require.True(t, replicaReady)
	require.Equal(t, "2", run(t, "GET", "counter"), "stream drains must not duplicate previous commands")
	require.Equal(t, value, run(t, "GET", "large"))
	gotTTL, ok := dictStore.GetExpiry("large")
	require.True(t, ok)
	require.Equal(t, wantTTL, gotTTL)
	require.EqualValues(t, 0, run(t, "SCARD", "set"))
	gotCMS, ok := dumpKey("cms")
	require.True(t, ok)
	require.Equal(t, cms, gotCMS, "opaque updates preserve the exact image")
	assertCheckpointDigest(t, path)
}

func TestAOFTranscriptExpiryAndRecreationKeepCanonicalOrder(t *testing.T) {
	setupReplicationV2(t)
	oldSamples, oldRounds := config.ActiveExpireSamples, config.ActiveExpireRounds
	config.ActiveExpireSamples, config.ActiveExpireRounds = 64, 4
	t.Cleanup(func() { config.ActiveExpireSamples, config.ActiveExpireRounds = oldSamples, oldRounds })
	keys := make([]string, 40)
	for i := range keys {
		keys[i] = fmt.Sprint(i) + strings.Repeat("e", 128<<10)
		run(t, "SET", keys[i], "old")
		dictStore.SetExpiryAt(keys[i], 1)
	}
	// These direct expiries model a clock advance after persistence without a
	// sleep. Snapshot expiry semantics are covered by the full replication suite.
	run(t, "SET", keys[0], "new")
	for KeysWithExpiry() > 0 {
		require.Positive(t, ExpireCycle())
	}
	require.Equal(t, "new", run(t, "GET", keys[0]))
	require.EqualValues(t, 1, run(t, "DBSIZE"))
	require.NoError(t, FlushAOF())
	path := aof.path
	require.NoError(t, CloseAOF())
	for i := 0; i < 2; i++ {
		restart(t, path)
		require.EqualValues(t, 1, run(t, "DBSIZE"))
		require.Equal(t, "new", run(t, "GET", keys[0]))
	}
	var body []byte
	for _, piece := range replicationV2.history {
		body = append(body, piece.body...)
	}
	file, err := os.ReadFile(path)
	require.NoError(t, err)
	require.True(t, bytes.Equal(file, body), "expiry drains publish each canonical byte exactly once")
}

func TestAOFTranscriptJoinsOlderAppendBeforeDirectDrain(t *testing.T) {
	ResetStores()
	oldWrite := aofWrite
	release, entered := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }); CloseAOF(); aofWrite = oldWrite; ResetStores() })
	path := filepath.Join(t.TempDir(), "store.aof")
	require.NoError(t, OpenAOF(path))
	var calls atomic.Int32
	var firstDone atomic.Bool
	aofWrite = func(f *os.File, body []byte) (int, error) {
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
	run(t, "SET", "first", "safe")
	ready, err := FlushAOFAsync(nil)
	require.NoError(t, err)
	require.False(t, ready)
	<-entered
	require.False(t, AppendHasRoom(2*maxAOFTranscriptBytes), "normal server admission must use its barrier")
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
	run(t, "SET", "second", value) // even a direct core caller must preserve order
	require.True(t, <-joined, "direct drain did not exercise the blocked append join")
	require.NoError(t, aof.failed)
	require.NoError(t, CloseAOF())
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	want := appendCommand(nil, "SET", "first", "safe")
	want = appendCommand(want, "SET", "second", value)
	require.True(t, bytes.Equal(want, got), "older append is the exact first prefix")
}

func TestAOFTranscriptMassEvictionKeepsBoundedBuffer(t *testing.T) {
	oldLimit, oldMemory := config.KeyNumberLimit, config.MaxMemory
	oldPolicy := config.AOFFsync
	config.AOFFsync = config.FsyncNever
	config.KeyNumberLimit, config.MaxMemory = 1000, 0
	t.Cleanup(func() {
		CloseAOF()
		config.KeyNumberLimit, config.MaxMemory, config.AOFFsync = oldLimit, oldMemory, oldPolicy
		ResetStores()
	})
	ResetStores()
	path := filepath.Join(t.TempDir(), "eviction.aof")
	require.NoError(t, OpenAOF(path))
	keys := make([]string, 0, 65)
	for i := 0; i < 64; i++ {
		key := fmt.Sprint(i) + strings.Repeat("k", 128<<10)
		keys = append(keys, key)
		require.Equal(t, "OK", run(t, "SET", key, "v"))
		require.NoError(t, FlushAOF())
	}
	keys = append(keys, "last")
	config.KeyNumberLimit = 1
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	require.Equal(t, "OK", run(t, "SET", "last", "v"))
	runtime.ReadMemStats(&after)
	t.Logf("mass eviction allocated %d bytes, retained log capacity %d", after.TotalAlloc-before.TotalAlloc, cap(aof.buf))
	require.LessOrEqual(t, cap(aof.buf), maxAOFTranscriptBytes)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(8<<20))
	var survivors []string
	for _, key := range keys {
		if dictStore.Has(key) {
			survivors = append(survivors, key)
		}
	}
	require.Len(t, survivors, 1)
	require.NoError(t, CloseAOF())
	config.KeyNumberLimit = 1000
	for i := 0; i < 2; i++ {
		restart(t, path)
		require.EqualValues(t, 1, run(t, "DBSIZE"))
		require.Equal(t, "v", run(t, "GET", survivors[0]))
	}
}
