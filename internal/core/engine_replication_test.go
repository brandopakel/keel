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

// replicaEngine is an engine of its own that follows a primary over protocol,
// with a log in dir, as a server started with -replicaof is.
func replicaEngine(t *testing.T, dir, name string, protocol int) *Engine {
	t.Helper()
	e := newEngine(data_structure.NewSpace(engineLimits(math.MaxInt)))
	e.ownRole = replicationRole{ReplicaOf: "primary.test:6379", Protocol: protocol}
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
	assert.Equal(t, replicaCheckpoint{Version: 2, Primary: e.replicaOf(), Epoch: epoch, Offset: offset,
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
// The primaries' streams are written first, by the default engine, so that
// the replicas can be applied in any order; TestEnginesShareNoReplication
// runs primaries of their own.
func TestEnginesShareNoReplica(t *testing.T) {
	never := goldenWindow{start: -1, end: -1 - int64(24*time.Hour/time.Millisecond)}
	setupReplicationV2(t)
	streams := []replicationStream{captureStreamV2(t, "a"), captureStreamV2(t, "b")}
	sa, sb := streams[0], streams[1]
	require.NotEqual(t, sa.epoch, sb.epoch)

	// The default engine is a replica from here too.
	require.NoError(t, CloseAOF())
	ResetStores()
	config.ReplicationFeed, config.ReplicaOf = false, "primary.test:6379"
	require.NoError(t, InitReplication())
	dir := t.TempDir()
	a, b := replicaEngine(t, dir, "a", 2), replicaEngine(t, dir, "b", 2)
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
	c := replicaEngine(t, dir, "c", 2)
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
	a2, b2 := replicaEngine(t, dir, "a2", 2), replicaEngine(t, dir, "b2", 2)
	var wg sync.WaitGroup
	for _, side := range []struct {
		e *Engine
		s replicationStream
	}{{a2, sa}, {b2, sb}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, f := range side.s.frames {
				if !assert.NoError(t, side.e.ApplyReplication(f)) {
					return
				}
				var w replyWriter
				if !assert.NoError(t, side.e.evalAndResponse(&Command{Cmd: "GET", Args: []string{side.s.name + ":after"}}, &w)) {
					return
				}
				// A replica serves reads once a frame says it has caught up.
				if !f.CaughtUp {
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
		restarted.ownRole = side.e.ownRole
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
	a1, b1 := replicaEngine(t, dir, "a1", 1), replicaEngine(t, dir, "b1", 1)
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

// pullFrom runs a replica's pull on primary p, as the transport sends it, and
// decodes the frame it answers.
func pullFrom(t *testing.T, p *Engine, name string, args ...string) (ReplicationFrame, bool) {
	t.Helper()
	var w replyWriter
	if !assert.NoError(t, p.evalAndResponse(&Command{Cmd: name, Args: args}, &w)) {
		return ReplicationFrame{}, false
	}
	reply, _ := Decode(w.b)
	encoded, ok := reply.(string)
	if !assert.True(t, ok, "%s: %q", name, w.b) {
		return ReplicationFrame{}, false
	}
	var f ReplicationFrame
	return f, assert.NoError(t, json.Unmarshal([]byte(encoded), &f))
}

// follow is a replica's transport, in-process: r pulls from p and applies
// what it is sent until it has caught up, driving a snapshot's rewrite on p
// when one is asked for, as p's loop would. cursor is where r is in p's
// stream, as the transport keeps it between pulls.
type follow struct {
	p, r                *Engine
	protocol            int
	epoch, snapshotID   string
	offset, snapshotPos uint64
}

func (f *follow) catchUp(t *testing.T) bool {
	t.Helper()
	for i := 0; i < 10000; i++ {
		if f.protocol == 1 {
			frame, ok := pullFrom(t, f.p, "KEEL.REPL.PULL", f.epoch, strconv.FormatUint(f.offset, 10))
			if !ok || !assert.NoError(t, f.r.ApplyReplication(frame)) {
				return false
			}
			level := !frame.Full && frame.To == frame.From
			f.epoch, f.offset = frame.Epoch, frame.To
			if level {
				return true
			}
			continue
		}
		// The term-aware form, with the term the replica knows, which a
		// primary that holds one requires.
		frame, ok := pullFrom(t, f.p, "KEEL.REPL.PULL2", f.epoch, strconv.FormatUint(f.offset, 10),
			f.snapshotID, strconv.FormatUint(f.snapshotPos, 10), strconv.FormatUint(f.r.CurrentTerm(), 10))
		if !ok || !assert.NoError(t, f.r.ApplyReplication(frame)) {
			return false
		}
		switch {
		case frame.Full && frame.Pending:
			f.epoch, f.offset, f.snapshotID, f.snapshotPos = "", 0, "", 0
			for j := 0; f.p.RewriteActive(); j++ {
				if !assert.NoError(t, f.p.FlushAOF()) || !assert.Less(t, j, 100000, "the snapshot did not end") {
					return false
				}
				time.Sleep(20 * time.Microsecond)
			}
		case frame.Full && frame.SnapshotDone:
			f.epoch, f.offset, f.snapshotID, f.snapshotPos = frame.Epoch, frame.To, "", 0
		case frame.Full:
			f.epoch, f.snapshotID, f.snapshotPos = frame.Epoch, frame.SnapshotID, frame.SnapshotOffset+uint64(len(frame.Body))
		default:
			f.epoch, f.offset = frame.Epoch, frame.To
			if frame.CaughtUp {
				return true
			}
		}
	}
	t.Errorf("%s did not catch up", f.r.aof.path)
	return false
}

// TestEnginesShareNoReplication: each engine feeds its replicas a stream of
// its own writes, in an epoch of its own, snapshots its own log for them, and
// hears its own replicas' acknowledgements, as its own role says. Three
// primaries in one process, two over protocol 2 and one over protocol 1, each
// with a replica of its own that pulls from it in-process and applies what it
// is sent, run side by side on goroutines of their own: each replica ends
// with its own primary's keyspace, and no primary's stream, snapshot, epoch
// or acknowledgement is another's. Then one primary's epoch starts again and
// one primary's log is closed, and the others carry on. The default engine,
// neither a primary nor a replica, feeds nothing and applies nothing. Under
// -race, stream state the engines shared would fail here.
func TestEnginesShareNoReplication(t *testing.T) {
	never := goldenWindow{start: -1, end: -1 - int64(24*time.Hour/time.Millisecond)}
	ResetStores()
	defaultEpoch := defaultEngine.replication.epoch
	dir := t.TempDir()
	primary := func(name string, protocol int) *Engine {
		e := newEngine(data_structure.NewSpace(engineLimits(math.MaxInt)))
		e.ownRole = replicationRole{Feed: true, Protocol: protocol}
		require.NoError(t, e.OpenAOF(filepath.Join(dir, name+".aof")))
		require.NoError(t, e.InitReplication())
		t.Cleanup(func() { e.CloseAOF() })
		return e
	}
	pairs := []*follow{
		{p: primary("pa", 2), r: replicaEngine(t, dir, "ra", 2), protocol: 2},
		{p: primary("pb", 2), r: replicaEngine(t, dir, "rb", 2), protocol: 2},
		{p: primary("pc", 1), r: replicaEngine(t, dir, "rc", 1), protocol: 1},
	}
	assert.Equal(t, "-READONLY You can't write against a read only replica.\r\n",
		string(rawOn(t, pairs[0].r, "SET", "k", "v")), "a replica's role is its own")
	assert.Equal(t, "OK", on(t, pairs[0].p, "SET", "k", "v"), "and so is a primary's")
	on(t, pairs[0].p, "DEL", "k")

	// Side by side: each primary writes the same names, as types of its own -
	// k is a filter with an expiry on one, a string on another, a hash on the
	// third - with transactions and a key reaped lazily, and its replica
	// catches up after each round.
	var wg sync.WaitGroup
	for n, pair := range pairs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := pair.p
			do := func(parts ...string) bool {
				var w replyWriter
				err := p.evalAndResponse(&Command{Cmd: parts[0], Args: parts[1:]}, &w)
				return assert.NoError(t, err) && assert.NotEqual(t, byte('-'), w.b[0], "%q: %q", parts, w.b)
			}
			for round := range 4 {
				v := strconv.Itoa(n) + ":" + strconv.Itoa(round)
				ok := do("SET", "s:"+strconv.Itoa(round), v) && do("INCR", "count") && do("RPUSH", "l", v) &&
					do("SADD", "set", v) && do("PFADD", "hll", v) && do("SET", "brief", v, "PX", "1")
				switch n {
				case 0:
					ok = ok && do("BF.ADD", "k", v) && do("PEXPIRE", "k", "3600000")
				case 1:
					ok = ok && do("SET", "k", v)
				default:
					ok = ok && do("HSET", "k", "f", v)
				}
				var tx *Transaction
				for _, cmd := range [][]string{{"MULTI"}, {"SET", "tx", v}, {"LPOP", "l"}, {"CMS.INITBYDIM", "cms:" + v, "10", "2"}, {"EXEC"}} {
					var w replyWriter
					var err error
					if tx, err = p.transact(tx, &Command{Cmd: cmd[0], Args: cmd[1:]}, &w, nil); !assert.NoError(t, err) {
						return
					}
				}
				time.Sleep(2 * time.Millisecond)
				// Reaped: the read answers nil and logs the key's DEL, which
				// the stream carries to the replica.
				var w replyWriter
				ok = ok && assert.NoError(t, p.evalAndResponse(&Command{Cmd: "GET", Args: []string{"brief"}}, &w)) &&
					assert.Equal(t, "$-1\r\n", string(w.b), "brief was reaped")
				if !ok || !assert.NoError(t, p.FlushAOF()) || !pair.catchUp(t) {
					return
				}
			}
		}()
	}
	wg.Wait()
	if t.Failed() {
		return
	}
	epochs := map[string]bool{defaultEpoch: true}
	for _, pair := range pairs {
		p, r := pair.p, pair.r
		assert.Equal(t, string(engineState(t, p, never)), string(engineState(t, r, never)), "%s holds its primary's keyspace", r.aof.path)
		assert.False(t, epochs[p.replication.epoch], "%s's epoch is its own", p.aof.path)
		epochs[p.replication.epoch] = true
		assert.Equal(t, p.replication.epoch, r.replicaEpoch)
		if pair.protocol == 2 {
			assert.Equal(t, p.replicationV2.end, r.replicaOffset)
			// A pull acknowledges the cursor it asks from, so the replica's
			// next one, level with the stream, acknowledges its end.
			require.True(t, pair.catchUp(t))
			assert.Equal(t, p.replicationV2.end, p.replicaAck.offset, "%s heard its own replica", p.aof.path)
			assert.NotNil(t, p.replicationV2.snapshot, "%s served a snapshot of its own log", p.aof.path)
		} else {
			assert.Equal(t, p.replication.offset, r.replicaOffset)
			assert.Empty(t, p.replication.dirty)
		}
	}
	assert.NotEqual(t, pairs[0].p.replicationV2.end, pairs[1].p.replicationV2.end, "each stream holds its own writes")

	// a's epoch starts again: a's replica takes a new snapshot of a, and b's
	// stream, epoch and replica carry on.
	pa, pb := pairs[0].p, pairs[1].p
	bEpoch, bEnd := pb.replication.epoch, pb.replicationV2.end
	aEpoch := pa.replication.epoch
	pa.replication.invalidated = true
	require.Equal(t, "OK", on(t, pa, "SET", "after", "a"))
	assert.NotEqual(t, aEpoch, pa.replication.epoch)
	assert.Equal(t, bEpoch, pb.replication.epoch)
	assert.Equal(t, bEnd, pb.replicationV2.end)
	require.True(t, pairs[0].catchUp(t))
	assert.Equal(t, string(engineState(t, pa, never)), string(engineState(t, pairs[0].r, never)))
	require.Equal(t, "OK", on(t, pb, "SET", "after", "b"))
	require.NoError(t, pb.FlushAOF())
	require.True(t, pairs[1].catchUp(t))
	assert.Equal(t, string(engineState(t, pb, never)), string(engineState(t, pairs[1].r, never)))

	// Closing a's log closes a's snapshot, and b's stays open for b's
	// replicas.
	require.NoError(t, pa.CloseAOF())
	assert.Nil(t, pa.replicationV2.snapshot)
	require.NotNil(t, pb.replicationV2.snapshot)
	_, err := pb.replicationV2.snapshot.ReadAt(make([]byte, 1), 0)
	assert.NoError(t, err, "b's snapshot is still open")

	// The default engine fed none of it and applied none of it.
	assert.Equal(t, defaultEpoch, defaultEngine.replication.epoch)
	assert.Zero(t, defaultEngine.replicationV2.end)
	assert.Zero(t, defaultEngine.replication.offset)
	assert.Empty(t, defaultEngine.replication.dirty)
	assert.Nil(t, defaultEngine.replicationV2.snapshot)
	assert.Zero(t, defaultEngine.replicaAck)
	assert.False(t, defaultEngine.replicaReady)
	assert.Zero(t, defaultEngine.space.TotalKeys())
}

// TestEnginesShareNoFailover: each engine holds its own term, kept in a file
// beside its own log through I/O of its own. Promoting, fencing or a term
// learned from a peer on one engine leaves every other engine's writes, term
// file and frames alone; one engine's failing disk fences that engine alone;
// a restart reads each engine's own term back; and a replica learns only its
// own primary's term. Side by side on goroutines, with another goroutine
// reading each engine's term as the replica transport does, under -race,
// term state the engines shared would fail here.
func TestEnginesShareNoFailover(t *testing.T) {
	ResetStores()
	defaultTerm, defaultPath := defaultEngine.CurrentTerm(), defaultEngine.failover.path
	engine := func(dir string, role replicationRole) (*Engine, string) {
		e := newEngine(data_structure.NewSpace(engineLimits(math.MaxInt)))
		e.ownRole = role
		path := filepath.Join(dir, "store.aof")
		require.NoError(t, e.LoadTerm(path))
		require.NoError(t, e.OpenAOF(path))
		require.NoError(t, e.InitReplication())
		t.Cleanup(func() { e.CloseAOF() })
		return e, path
	}
	termFile := func(path string) string {
		body, err := os.ReadFile(path + termFileName)
		if os.IsNotExist(err) {
			return "none"
		}
		require.NoError(t, err)
		return string(body)
	}
	feed := replicationRole{Feed: true, Protocol: 2}
	follow2 := replicationRole{ReplicaOf: "primary.test:6379", Protocol: 2}
	a, aPath := engine(t.TempDir(), feed)
	b, bPath := engine(t.TempDir(), feed)

	// a is promoted, b fenced at a higher term: each holds its own.
	require.Equal(t, "OK", on(t, a, "KEEL.PROMOTE", "3"))
	require.Equal(t, "OK", on(t, b, "KEEL.FENCE", "7"))
	assert.Equal(t, [3]uint64{3, 3, 7}, [3]uint64{a.CurrentTerm(), a.failover.held, b.CurrentTerm()})
	assert.True(t, a.writable())
	assert.False(t, b.writable())
	assert.Equal(t, "OK", on(t, a, "SET", "k", "a"))
	assert.Equal(t, "-FENCED this node is not the holder of the current term\r\n", string(rawOn(t, b, "SET", "k", "b")))
	assert.Equal(t, "3", termFile(aPath))
	assert.Equal(t, "7", termFile(bPath))
	assert.Contains(t, on(t, a, "INFO", "replication"), "failover_term:3\r\nfailover_held_term:3\r\nfailover_fenced:false\r\nwritable:true\r\n")
	assert.Contains(t, on(t, b, "INFO", "replication"), "failover_term:7\r\nfailover_held_term:0\r\nfailover_fenced:true\r\nwritable:false\r\n")

	// a's frames carry a's term; a replica of a learns it, beside its own
	// log; a fenced b serves no frames.
	ra, raPath := engine(t.TempDir(), follow2)
	f := &follow{p: a, r: ra, protocol: 2}
	require.True(t, f.catchUp(t))
	assert.Equal(t, uint64(3), ra.CurrentTerm())
	assert.Equal(t, "3", termFile(raPath))
	assert.Equal(t, "-FENCED this node is not the holder of the current term\r\n",
		string(rawOn(t, b, "KEEL.REPL.PULL2", "", "0", "", "0", "7")))
	// A replica of b's that has moved on to term 9 deposes b, and only b.
	assert.Equal(t, "-FENCED this node is not the holder of the current term\r\n",
		string(rawOn(t, b, "KEEL.REPL.PULL2", "", "0", "", "0", "9")))
	assert.Equal(t, "9", termFile(bPath))
	assert.Equal(t, uint64(3), a.CurrentTerm())
	assert.Equal(t, "3", termFile(aPath))

	// a's disk will not sync its term file: promoting a fails and fences a,
	// through a's I/O alone, and b's promotion goes through b's.
	var aSyncs, bSyncs atomic.Int64
	diskErr := errors.New("a's disk will not sync")
	a.termSync = func(*os.File) error { aSyncs.Add(1); return diskErr }
	b.termSync = func(f *os.File) error { bSyncs.Add(1); return f.Sync() }
	assert.Equal(t, "-ERR persisting term: a's disk will not sync\r\n", string(rawOn(t, a, "KEEL.PROMOTE", "4")))
	assert.False(t, a.writable(), "a term a cannot keep is still one a has seen")
	assert.Equal(t, "3", termFile(aPath))
	assert.Equal(t, "OK", on(t, b, "KEEL.PROMOTE", "10"))
	assert.Equal(t, "10", termFile(bPath))
	assert.True(t, b.writable())
	assert.Equal(t, int64(1), aSyncs.Load())
	assert.Equal(t, int64(1), bSyncs.Load())
	a.termSync = syncFile
	assert.Equal(t, "OK", on(t, a, "KEEL.PROMOTE", "5"))
	assert.True(t, a.writable())

	// Side by side: each engine takes terms and writes on a goroutine of its
	// own, while another goroutine reads its term as the transport does. The
	// readers stop only once both writers have finished, so reads overlap
	// every update, the last ones included.
	var readers, writers sync.WaitGroup
	stop := make(chan struct{})
	for _, e := range []*Engine{a, b} {
		readers.Add(1)
		writers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = e.CurrentTerm()
				}
			}
		}()
		go func() {
			defer writers.Done()
			base := e.CurrentTerm()
			for i := uint64(1); i <= 50; i++ {
				var w replyWriter
				for _, cmd := range []*Command{
					{Cmd: "KEEL.PROMOTE", Args: []string{strconv.FormatUint(base+2*i, 10)}},
					{Cmd: "SET", Args: []string{"k", strconv.FormatUint(i, 10)}},
					{Cmd: "KEEL.FENCE", Args: []string{strconv.FormatUint(base+2*i+1, 10)}},
				} {
					w.b = w.b[:0]
					if !assert.NoError(t, e.evalAndResponse(cmd, &w)) || !assert.Equal(t, "+OK\r\n", string(w.b), "%s", cmd.Cmd) {
						return
					}
				}
			}
		}()
	}
	writers.Wait()
	close(stop)
	readers.Wait()
	aTerm, bTerm := a.CurrentTerm(), b.CurrentTerm()
	assert.Equal(t, uint64(5+101), aTerm)
	assert.Equal(t, uint64(10+101), bTerm)
	assert.True(t, a.failover.fenced)
	assert.True(t, b.failover.fenced)
	assert.Equal(t, strconv.FormatUint(aTerm, 10), termFile(aPath))
	assert.Equal(t, strconv.FormatUint(bTerm, 10), termFile(bPath))

	// Restarted, each reads its own term back, and starts fenced.
	for _, side := range []struct {
		path string
		term uint64
	}{{aPath, aTerm}, {bPath, bTerm}} {
		restarted := newEngine(data_structure.NewSpace(engineLimits(math.MaxInt)))
		restarted.ownRole = feed
		require.NoError(t, restarted.LoadTerm(side.path))
		assert.Equal(t, side.term, restarted.CurrentTerm())
		assert.False(t, restarted.writable())
	}
	assert.Equal(t, uint64(3), ra.CurrentTerm(), "a's replica learned only what a sent it")

	// The default engine saw none of it.
	assert.Equal(t, defaultTerm, defaultEngine.CurrentTerm())
	assert.Equal(t, defaultPath, defaultEngine.failover.path)
	assert.Equal(t, defaultTerm == 0, Writable() || config.ReplicaOf != "")
}
