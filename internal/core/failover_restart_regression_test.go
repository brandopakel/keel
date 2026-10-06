package core

import (
	"os"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestObservedTermCannotGrantAuthorityAfterRestart(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := setupFailover(t, e)
	require.Equal(t, "OK", runOn(t, e, "KEEL.PROMOTE", "1"))
	require.Equal(t, "OK", runOn(t, e, "KEEL.FENCE", "2"))
	require.False(t, e.writable())
	require.NoError(t, e.CloseAOF())
	require.NoError(t, e.LoadTerm(path))
	require.False(t, e.writable(), "an observed successor term must not become this node's write authority on restart")
	require.Equal(t, errFenced.Error(), runOn(t, e, "SET", "after-restart", "must-refuse"))
}

func TestFailedTermObservationStaysFencedAndRetriesPersistence(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	setupFailover(t, e)
	require.Equal(t, "OK", runOn(t, e, "KEEL.PROMOTE", "1"))
	oldSync := e.termSync
	t.Cleanup(func() { e.termSync = oldSync })
	e.termSync = func(*os.File) error { return os.ErrPermission }
	for i := 0; i < 2; i++ {
		require.Contains(t, runOn(t, e, "KEEL.FENCE", "5"), "recording term")
		require.Equal(t, uint64(5), e.CurrentTerm())
		require.Equal(t, uint64(1), e.failover.persisted)
		require.False(t, e.writable())
		require.Equal(t, errFenced.Error(), runOn(t, e, "SET", "blocked", "value"))
	}
	e.termSync = oldSync
	require.Equal(t, "OK", runOn(t, e, "KEEL.FENCE", "5"))
	require.Equal(t, uint64(5), e.failover.persisted)
	require.False(t, e.writable(), "persisting an observation does not grant authority")
	require.Equal(t, "OK", runOn(t, e, "KEEL.PROMOTE", "6"))
	require.True(t, e.writable())
}

func TestReplicaIsNeverReportedWritable(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	setupFailover(t, e)
	reconfigure(t, e, func(o *Options) { o.ReplicaOf = "primary.invalid:6379" })
	require.False(t, e.writable())
}

func TestPromotionWithoutTermStorageDoesNotFenceVolatileCache(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	setupFailover(t, e)
	require.NoError(t, e.CloseAOF())
	e.failover = failoverState{}
	require.Contains(t, runOn(t, e, "KEEL.PROMOTE", "42"), "requires an append-only log")
	require.Zero(t, e.CurrentTerm())
	require.True(t, e.writable())
	require.Equal(t, "OK", runOn(t, e, "SET", "still-available", "value"))
	// FENCE conveys an observation, so it still stops the live node even when
	// it cannot promise durable recovery of that observation.
	require.Contains(t, runOn(t, e, "KEEL.FENCE", "42"), "recording term")
	require.False(t, e.writable())
}

func TestNonzeroTermsCannotUseProtocol1(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	setupFailover(t, e)
	reconfigure(t, e, func(o *Options) { o.ReplicationProtocol, o.ReplicationFeed = 1, true })
	require.NoError(t, e.InitReplication())
	require.Equal(t, "OK", runOn(t, e, "KEEL.PROMOTE", "1"))
	require.Contains(t, runOn(t, e, "KEEL.REPL.PULL", "", "0"), "require replication protocol 2")
	e.replicaReady = true
	require.ErrorContains(t, e.ApplyReplication(ReplicationFrame{Version: 1}), "require replication protocol 2")
	require.False(t, e.replicaReady)
}

func TestCurrentTermCanBeReadByTransport(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	setupFailover(t, e)
	ready, stop, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	observed := make(chan uint64, 1)
	go func() {
		defer close(done)
		close(ready)
		for {
			select {
			case <-stop:
				observed <- e.CurrentTerm()
				return
			default:
				_ = e.CurrentTerm()
				runtime.Gosched()
			}
		}
	}()
	<-ready
	for term := uint64(1); term <= 20; term++ {
		if err := e.observeTerm(term); err != nil {
			close(stop)
			<-done
			t.Fatal(err)
		}
	}
	close(stop)
	<-done
	require.Equal(t, uint64(20), <-observed)
}

func TestProtocol2TermZeroAcceptsLegacyPull(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	setupReplicationV2On(t, e)
	old := e.failover
	e.failover = failoverState{}
	t.Cleanup(func() { e.failover = old })
	reply := runOn(t, e, "KEEL.REPL.PULL2", "", "0", "", "0")
	require.Contains(t, reply, `"version":2`, "term-zero peers retain the existing protocol-2 request shape")
}

func TestProtocol2NonzeroTermRequiresExplicitCapability(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	setupReplicationV2On(t, e)
	old := e.failover
	e.failover = failoverState{term: 1, persisted: 1, held: 1}
	t.Cleanup(func() { e.failover = old })
	require.Equal(t, []byte(ReplicationTermRequiredReply), e.cmdReplicationPullV2([]string{"", "0", "", "0"}))
	require.Contains(t, runOn(t, e, "KEEL.REPL.PULL2", "", "0", "", "0", "0"), `"term":1`)
	require.True(t, e.writable())
}
