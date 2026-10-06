package core

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// setupFailover gives each test its own log directory and term file, and puts
// the default engine's failover state back afterwards.
func setupFailover(t *testing.T, e *Engine) string {
	t.Helper()
	oldState := e.failover
	t.Cleanup(func() {
		e.CloseAOF()
		e.failover = oldState
	})
	e.resetStores()
	withOptionsOn(t, e, func(o *Options) { o.ReplicaOf = "" })
	path := filepath.Join(t.TempDir(), "term.aof")
	require.NoError(t, e.LoadTerm(path))
	require.NoError(t, e.OpenAOF(path))
	return path
}

func TestFreshNodeWritesAtTermZero(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	setupFailover(t, e)
	require.True(t, e.writable(), "a node that has never been promoted still serves")
	require.Equal(t, uint64(0), e.CurrentTerm())
	require.Equal(t, "OK", runOn(t, e, "SET", "k", "v"))
}

func TestPromotionRequiresAHigherTermAndSurvivesRestart(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := setupFailover(t, e)

	require.Equal(t, "OK", runOn(t, e, "KEEL.PROMOTE", "7"))
	require.Equal(t, uint64(7), e.CurrentTerm())
	require.Equal(t, uint64(7), e.failover.held)
	require.True(t, e.writable())

	// A node that could reuse or lower a term could outrank a leader chosen
	// properly, so both are refused.
	require.Contains(t, runOn(t, e, "KEEL.PROMOTE", "7"), "not above the current term")
	require.Contains(t, runOn(t, e, "KEEL.PROMOTE", "3"), "not above the current term")
	require.Equal(t, uint64(7), e.CurrentTerm(), "a refused promotion changes nothing")

	// The term is on disk before it is acted on, so a restart cannot walk it
	// back into one the cluster has already left.
	require.NoError(t, e.CloseAOF())
	e.failover = failoverState{}
	require.NoError(t, e.LoadTerm(path))
	require.Equal(t, uint64(7), e.CurrentTerm(), "the term must survive a restart")
	require.Zero(t, e.failover.held, "observed terms do not grant authority to a new incarnation")
	require.False(t, e.writable())
	require.Equal(t, "OK", runOn(t, e, "KEEL.PROMOTE", "8"))
	require.True(t, e.writable(), "a fresh externally assigned higher term is required")
}

// The property the whole design exists for: a node that is not the holder of
// the highest term it has seen does not take a write.
func TestFencingRefusesWritesImmediatelyAndKeepsServingReads(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	setupFailover(t, e)
	require.Equal(t, "OK", runOn(t, e, "KEEL.PROMOTE", "2"))
	require.Equal(t, "OK", runOn(t, e, "SET", "k", "before"))

	// A term this node does not hold now exists. It stands down at once rather
	// than finishing what it was doing.
	require.Equal(t, "OK", runOn(t, e, "KEEL.FENCE", "3"))
	require.True(t, e.failover.fenced)
	require.False(t, e.writable())

	for _, write := range [][]string{
		{"SET", "k", "after"}, {"DEL", "k"}, {"INCR", "n"},
		{"HSET", "h", "f", "v"}, {"RPUSH", "l", "v"}, {"SADD", "s", "m"},
	} {
		require.Equal(t, errFenced.Error(), runOn(t, e, write[0], write[1:]...),
			"%v must be refused by a fenced node", write)
	}
	// Reads continue: a deposed node holds stale data, not no data, and a
	// client asking for it is better served than disconnected.
	require.Equal(t, "before", runOn(t, e, "GET", "k"))

	// Only a term above the one it has seen restores it, which is what a
	// coordinator handing it leadership again would do.
	require.Equal(t, "OK", runOn(t, e, "KEEL.PROMOTE", "4"))
	require.False(t, e.failover.fenced)
	require.Equal(t, "OK", runOn(t, e, "SET", "k", "after"))
}

func TestFencingSurvivesRestart(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := setupFailover(t, e)
	require.Equal(t, "OK", runOn(t, e, "KEEL.PROMOTE", "2"))
	require.Equal(t, "OK", runOn(t, e, "KEEL.FENCE", "5"))
	require.False(t, e.writable())

	require.NoError(t, e.CloseAOF())
	e.failover = failoverState{}
	require.NoError(t, e.LoadTerm(path))
	// A restart retains the observation and must never claim the successor's
	// authority while waiting for a message that may never arrive.
	require.Equal(t, uint64(5), e.CurrentTerm())
	require.Zero(t, e.failover.held)
	require.True(t, e.failover.fenced)
	require.False(t, e.writable())
}

func TestADamagedTermFileStopsEveryRole(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	dir := t.TempDir()
	path := filepath.Join(dir, "term.aof")
	require.NoError(t, os.WriteFile(path+termFileName, []byte("not-a-term"), 0o600))

	t.Cleanup(func() { e.failover = failoverState{} })

	reconfigure(t, e, func(o *Options) { o.ReplicaOf = "" })
	require.Error(t, e.LoadTerm(path),
		"a primary must not guess a term the cluster may already have moved past")

	// A replica must not forget the floor for rejecting stale histories.
	reconfigure(t, e, func(o *Options) { o.ReplicaOf = "primary.test:6379" })
	require.Error(t, e.LoadTerm(path))
}

func TestAMissingTermFileIsTermZero(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	dir := t.TempDir()
	t.Cleanup(func() { e.failover = failoverState{} })
	reconfigure(t, e, func(o *Options) { o.ReplicaOf = "" })
	// Every node before its first failover, and every fresh install. Refusing
	// to start on this would refuse to start on an upgrade.
	require.NoError(t, e.LoadTerm(filepath.Join(dir, "absent.aof")))
	require.Equal(t, uint64(0), e.CurrentTerm())
	require.True(t, e.writable())
}

// A term is only acted on once it is on disk. If the write cannot be made
// durable, the promotion fails rather than being taken on trust.
func TestPromotionFailsClosedWhenTheTermCannotBeMadeDurable(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	setupFailover(t, e)
	for _, broken := range []string{"sync", "rename", "dirsync"} {
		t.Run(broken, func(t *testing.T) {
			oldSync, oldRename, oldDir := e.termSync, e.termRename, e.termSyncDir
			defer func() {
				e.termSync, e.termRename, e.termSyncDir = oldSync, oldRename, oldDir
			}()
			failed := os.ErrPermission
			switch broken {
			case "sync":
				e.termSync = func(*os.File) error { return failed }
			case "rename":
				e.termRename = func(string, string) error { return failed }
			case "dirsync":
				e.termSyncDir = func(string) error { return failed }
			}
			before := e.CurrentTerm()
			reply := runOn(t, e, "KEEL.PROMOTE", strconv.FormatUint(before+10, 10))
			require.Contains(t, reply, "persisting term")
			require.Equal(t, before+10, e.CurrentTerm(), "remember the attempted term even on failure")
			require.False(t, e.writable(), "a failed authority update must stop writes")
			require.NotEqual(t, e.CurrentTerm(), e.failover.held, "a term that did not reach disk is not held")
		})
	}
}

func TestInfoReportsTheTerm(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	setupFailover(t, e)
	reconfigure(t, e, func(o *Options) { o.ReplicationFeed = true })
	require.Equal(t, "OK", runOn(t, e, "KEEL.PROMOTE", "11"))
	info, ok := runOn(t, e, "INFO", "replication").(string)
	require.True(t, ok)
	require.Contains(t, info, "failover_term:11")
	require.Contains(t, info, "failover_held_term:11")
	require.Contains(t, info, "writable:true")
}

// A term travels on every pull, so a primary that has been replaced finds out
// from the first replica that has moved on, rather than waiting for a
// coordinator to remember to tell it.
func TestAPullFromAHigherTermDeposesThePrimary(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	setupReplicationV2On(t, e)
	oldState := e.failover
	t.Cleanup(func() { e.failover = oldState })
	e.failover = failoverState{path: filepath.Join(t.TempDir(), "p.term")}

	runOn(t, e, "SET", "k", "v")
	require.Equal(t, "OK", runOn(t, e, "KEEL.PROMOTE", "4"))
	require.True(t, e.writable())

	// A pull at the term it holds is served, and the frame says which term the
	// data came from.
	frame := pullV2On(t, e, "", 0, "", 0)
	require.Equal(t, uint64(4), frame.Term)

	// A replica that has moved to term 5 pulls. The primary learns it has been
	// replaced and stands down, on the pull itself.
	reply := runOn(t, e, "KEEL.REPL.PULL2", "", "0", "", "0", "5")
	require.Equal(t, errFenced.Error(), reply,
		"a deposed primary must stop feeding replicas, not only stop taking writes")
	require.Equal(t, uint64(5), e.CurrentTerm())
	require.True(t, e.failover.fenced)
	require.False(t, e.writable())
	require.Equal(t, errFenced.Error(), runOn(t, e, "SET", "k", "after"))
}

// A replica must not follow a primary from a term it has already left, which
// would rewind it onto a history the cluster abandoned.
func TestAReplicaRefusesFramesFromAnOlderTerm(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	setupReplicationV2On(t, e)
	runOn(t, e, "SET", "k", "v")
	frames := snapshotV2On(t, e)
	base, epoch := frames[0].To, frames[0].Epoch

	path := becomeReplicaV2On(t, e)
	oldState := e.failover
	t.Cleanup(func() { e.failover = oldState })
	e.failover = failoverState{path: path + termFileName}

	for _, f := range frames {
		require.NoError(t, e.ApplyReplication(f))
	}
	// The replica learns of term 6 - the frames above carried none, so this is
	// the coordinator or a newer primary telling it.
	require.NoError(t, e.observeTerm(6))
	require.Equal(t, uint64(6), e.CurrentTerm())

	stale := signedV2(ReplicationFrame{Version: 2, Epoch: epoch, From: base, To: base, CaughtUp: true, Term: 5})
	err := e.ApplyReplication(stale)
	require.Error(t, err)
	require.Contains(t, err.Error(), "below the known term")
	require.False(t, e.replicaReady, "a rejected frame must not leave the replica readable")

	// Rejecting it also untrusts the replica, so it cannot simply carry on from
	// a frame that happens to carry an acceptable term afterwards. Recovering
	// means a fresh synchronisation, which is the same answer the checkpoint and
	// checksum guards give: a replica that has been lied to starts over.
	require.False(t, e.replicaV2.trusted)
	current := signedV2(ReplicationFrame{Version: 2, Epoch: epoch, From: base, To: base, CaughtUp: true, Term: 6})
	err = e.ApplyReplication(current)
	require.Error(t, err, "a delta must not resume after a rejected frame")
	require.False(t, e.replicaReady)
}
