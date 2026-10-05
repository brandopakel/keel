package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brandopakel/keel/internal/config"
	"github.com/brandopakel/keel/internal/data_structure"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// replicationStream is what a primary sent: its frames from the start, the
// epoch they belong to and where they end, and the keyspace the primary held
// at the end.
type replicationStream struct {
	name   string
	frames []ReplicationFrame
	epoch  string
	end    uint64
	state  string
}

// captureStreamV2 writes a primary's protocol 2 stream on the default engine,
// in an epoch of its own: keys named for the primary, a snapshot, then a
// hash, a filter's image, and a transaction larger than one frame, which a
// replica holds across frames until its EXEC arrives.
func captureStreamV2(t *testing.T, name string) replicationStream {
	t.Helper()
	never := goldenWindow{start: -1, end: -1 - int64(24*time.Hour/time.Millisecond)}
	ResetStores()
	require.NoError(t, InitReplication())
	for i := range 20 {
		run(t, "SET", name+":"+strconv.Itoa(i), name)
	}
	frames := snapshotV2(t)
	s := replicationStream{name: name, epoch: frames[0].Epoch}
	run(t, "HSET", name+":h", "f", name)
	run(t, "BF.ADD", name+":bf", name)
	session := &session{t: t}
	session.send("MULTI")
	session.send("SET", name+":first", strings.Repeat(name, 300<<10))
	session.send("INCR", name+":n")
	session.send("SET", name+":last", name)
	session.send("EXEC")
	run(t, "SET", name+":after", name)
	for offset := frames[0].To; ; {
		f := pullV2(t, s.epoch, offset, "", 0)
		frames = append(frames, f)
		offset = f.To
		if f.CaughtUp {
			s.end = offset
			break
		}
	}
	s.frames = frames
	s.state = string(engineState(t, defaultEngine, never))
	return s
}

// replicaEngine is an engine of its own that follows a primary, with a log in
// dir, as a server started with -replicaof is.
func replicaEngine(t *testing.T, dir, name string) *Engine {
	t.Helper()
	e := newEngine(data_structure.NewSpace(engineLimits(math.MaxInt)))
	require.NoError(t, e.OpenAOF(filepath.Join(dir, name+".aof")))
	require.NoError(t, e.followPrimary())
	t.Cleanup(func() { e.CloseAOF() })
	return e
}

// requireCheckpoint requires e's checkpoint to name e's own log and the
// position given.
func requireCheckpoint(t *testing.T, e *Engine, epoch string, offset uint64) {
	t.Helper()
	body, err := os.ReadFile(e.aof.path + ".replica-checkpoint")
	require.NoError(t, err)
	var cp replicaCheckpoint
	require.NoError(t, json.Unmarshal(body, &cp))
	log, err := os.ReadFile(e.aof.path)
	require.NoError(t, err)
	sum := sha256.Sum256(log)
	assert.Equal(t, replicaCheckpoint{Version: 2, Primary: config.ReplicaOf, Epoch: epoch, Offset: offset,
		Bytes: int64(len(log)), SHA256: hex.EncodeToString(sum[:])}, cp, "%s's checkpoint", e.aof.path)
}

// TestEnginesShareNoReplica: a replica applies its primary's stream to its
// own keyspace and its own log, holds its own place in that stream, and
// resumes from its own checkpoint. Two replicas in one process, each
// following a primary of its own, share none of it: one engine's frames,
// readiness, position, half-received transaction, checkpoint and failing disk
// stay its own, whether they apply one after the other, interleaved, or side
// by side through ApplyReplication, under protocols 2 and 1; and neither
// touches the default engine's replica. Under -race, replica state the
// engines shared would fail here.
//
// The primaries' streams are written first, by the default engine, because
// until plan step 2.4 moves the primary's side too a stream is the default
// engine's.
func TestEnginesShareNoReplica(t *testing.T) {
	never := goldenWindow{start: -1, end: -1 - int64(24*time.Hour/time.Millisecond)}
	setupReplicationV2(t)
	streams := []replicationStream{captureStreamV2(t, "a"), captureStreamV2(t, "b")}
	sa, sb := streams[0], streams[1]
	require.NotEqual(t, sa.epoch, sb.epoch)

	// Every engine in the process is a replica from here, the default one too.
	require.NoError(t, CloseAOF())
	ResetStores()
	config.ReplicationFeed, config.ReplicaOf = false, "primary.test:6379"
	require.NoError(t, InitReplication())
	dir := t.TempDir()
	a, b := replicaEngine(t, dir, "a"), replicaEngine(t, dir, "b")
	ready := func(e *Engine) bool {
		return strings.Contains(on(t, e, "INFO", "replication").(string), "replica_ready:1\r\n")
	}

	// Interleaved: a takes its snapshot and stops inside its transaction; b
	// takes all of its stream; then a finishes.
	held := -1
	for i, f := range sa.frames {
		require.NoError(t, a.ApplyReplication(f))
		if a.replicaV2.blockOpen {
			held = i
			break
		}
	}
	require.GreaterOrEqual(t, held, 0, "a's transaction spans frames")
	assert.Equal(t, "-MASTERDOWN replica has no recent primary state\r\n", string(rawOn(t, a, "GET", "a:0")))
	for _, f := range sb.frames {
		require.NoError(t, b.ApplyReplication(f))
	}
	assert.True(t, ready(b), "b caught up with its primary")
	assert.False(t, ready(a), "and a, inside its transaction, has not")
	assert.True(t, a.replicaV2.blockOpen, "a still holds its open transaction")
	assert.False(t, b.replicaV2.blockOpen)
	assert.Nil(t, a.dictStore.Peek("a:first"), "no part of a's transaction is visible before all of it")
	assert.Equal(t, sb.state, string(engineState(t, b, never)), "b holds its primary's keyspace")
	assert.Equal(t, sb.epoch, b.replicaEpoch)
	assert.Equal(t, sb.end, b.replicaOffset)
	requireCheckpoint(t, b, sb.epoch, sb.end)
	_, err := os.Stat(a.aof.path + ".replica-checkpoint")
	assert.True(t, os.IsNotExist(err), "a has not caught up, so it has no checkpoint")
	for _, f := range sa.frames[held+1:] {
		require.NoError(t, a.ApplyReplication(f))
	}
	assert.True(t, ready(a))
	assert.Equal(t, sa.state, string(engineState(t, a, never)), "a holds its primary's keyspace")
	assert.Equal(t, sa.epoch, a.replicaEpoch)
	assert.Equal(t, sa.end, a.replicaOffset)
	requireCheckpoint(t, a, sa.epoch, sa.end)
	requireCheckpoint(t, b, sb.epoch, sb.end)
	assert.Equal(t, "a", on(t, a, "GET", "a:after"))
	assert.Equal(t, "-READONLY You can't write against a read only replica.\r\n", string(rawOn(t, a, "SET", "a:0", "x")))
	for _, side := range []struct {
		e     *Engine
		other string
	}{{a, "b:"}, {b, "a:"}} {
		require.NoError(t, side.e.FlushAOF())
		log, err := os.ReadFile(side.e.aof.path)
		require.NoError(t, err)
		assert.NotContains(t, string(log), side.other, "%s holds only its own primary's writes", side.e.aof.path)
		assert.Contains(t, string(log), "*1\r\n$5\r\nMULTI\r\n", "%s frames its primary's transaction", side.e.aof.path)
	}

	// c follows a's primary on a disk that will not sync its checkpoint: c
	// stops, and neither a's checkpoint I/O nor b's is touched.
	c := replicaEngine(t, dir, "c")
	diskErr := errors.New("c's disk will not sync")
	c.checkpointSync = func(*os.File) error { return diskErr }
	var others atomic.Int64
	for _, e := range []*Engine{a, b} {
		e.checkpointSync = func(f *os.File) error { others.Add(1); return f.Sync() }
	}
	var applyErr error
	for _, f := range sa.frames {
		if applyErr = c.ApplyReplication(f); applyErr != nil {
			break
		}
	}
	require.ErrorIs(t, applyErr, diskErr)
	assert.False(t, ready(c))
	assert.Zero(t, others.Load(), "c's checkpoint went through c's I/O alone")
	assert.True(t, ready(a))
	assert.True(t, ready(b))

	// Side by side, on goroutines of their own: two fresh replicas each apply
	// their primary's stream, reading as they go.
	a2, b2 := replicaEngine(t, dir, "a2"), replicaEngine(t, dir, "b2")
	var wg sync.WaitGroup
	for _, side := range []struct {
		e *Engine
		s replicationStream
	}{{a2, sa}, {b2, sb}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i, f := range side.s.frames {
				if !assert.NoError(t, side.e.ApplyReplication(f)) {
					return
				}
				var w replyWriter
				if !assert.NoError(t, side.e.evalAndResponse(&Command{Cmd: "GET", Args: []string{side.s.name + ":after"}}, &w)) {
					return
				}
				if i < len(side.s.frames)-1 {
					assert.Equal(t, "-MASTERDOWN replica has no recent primary state\r\n", string(w.b))
				} else {
					assert.Equal(t, "$1\r\n"+side.s.name+"\r\n", string(w.b))
				}
				if !assert.NoError(t, side.e.FlushAOF()) {
					return
				}
			}
		}()
	}
	wg.Wait()
	for _, side := range []struct {
		e *Engine
		s replicationStream
	}{{a2, sa}, {b2, sb}} {
		assert.Equal(t, side.s.state, string(engineState(t, side.e, never)), "%s holds its primary's keyspace", side.s.name)
		requireCheckpoint(t, side.e, side.s.epoch, side.s.end)

		// Restarted on its own log, each resumes from its own checkpoint.
		path := side.e.aof.path
		require.NoError(t, side.e.CloseAOF())
		restarted := newEngine(data_structure.NewSpace(engineLimits(math.MaxInt)))
		_, err := restarted.LoadAOF(path)
		require.NoError(t, err)
		require.NoError(t, restarted.OpenAOF(path))
		t.Cleanup(func() { restarted.CloseAOF() })
		require.NoError(t, restarted.followPrimary())
		epoch, offset := restarted.ReplicaResumeCursor()
		assert.Equal(t, side.s.epoch, epoch, "%s resumes in its primary's epoch", path)
		assert.Equal(t, side.s.end, offset)
		assert.Equal(t, side.s.state, string(engineState(t, restarted, never)))
	}

	// The default engine, a replica as well, applied none of it.
	epoch, offset := ReplicaResumeCursor()
	assert.Equal(t, "", epoch)
	assert.Zero(t, offset)
	assert.False(t, defaultEngine.replicaReady)
	assert.Empty(t, defaultEngine.replicaEpoch)
	assert.Zero(t, defaultEngine.replicaUpdated)
	assert.False(t, defaultEngine.replicaV2.trusted)
	assert.Zero(t, defaultEngine.space.TotalKeys())

	// Protocol 1: two primaries' key images, applied side by side.
	config.ReplicationFeed, config.ReplicaOf, config.ReplicationProtocol = true, "", 1
	require.NoError(t, OpenAOF(filepath.Join(t.TempDir(), "primary1.aof")))
	var v1 []replicationStream
	for _, name := range []string{"a", "b"} {
		ResetStores()
		require.NoError(t, InitReplication())
		for i := range 20 {
			run(t, "SET", name+":"+strconv.Itoa(i), name)
		}
		s := replicationStream{name: name}
		pull := func(epoch string, offset uint64) ReplicationFrame {
			encoded, ok := run(t, "KEEL.REPL.PULL", epoch, strconv.FormatUint(offset, 10)).(string)
			require.True(t, ok)
			var f ReplicationFrame
			require.NoError(t, json.Unmarshal([]byte(encoded), &f))
			return f
		}
		full := pull("", 0)
		run(t, "HSET", name+":h", "f", name)
		run(t, "CMS.INITBYDIM", name+":cms", "10", "2")
		run(t, "DEL", name+":0")
		delta := pull(full.Epoch, full.To)
		s.frames, s.epoch, s.end = []ReplicationFrame{full, delta}, full.Epoch, delta.To
		s.state = string(engineState(t, defaultEngine, never))
		v1 = append(v1, s)
	}
	require.NoError(t, CloseAOF())
	ResetStores()
	config.ReplicationFeed, config.ReplicaOf = false, "primary.test:6379"
	require.NoError(t, InitReplication())
	a1, b1 := replicaEngine(t, dir, "a1"), replicaEngine(t, dir, "b1")
	for i, e := range []*Engine{a1, b1} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, f := range v1[i].frames {
				if !assert.NoError(t, e.ApplyReplication(f)) {
					return
				}
			}
		}()
	}
	wg.Wait()
	for i, e := range []*Engine{a1, b1} {
		assert.Equal(t, v1[i].state, string(engineState(t, e, never)), "%s holds its primary's keyspace", v1[i].name)
		assert.Equal(t, v1[i].epoch, e.replicaEpoch)
		assert.Equal(t, v1[i].end, e.replicaOffset)
		assert.True(t, e.replicaReady)
	}
	assert.False(t, defaultEngine.replicaReady)
	assert.Zero(t, defaultEngine.space.TotalKeys())
}
