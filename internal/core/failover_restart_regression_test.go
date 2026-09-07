package core

import (
	"os"
	"runtime"
	"testing"

	"github.com/brandopakel/keel/internal/config"
	"github.com/stretchr/testify/require"
)

func TestObservedTermCannotGrantAuthorityAfterRestart(t *testing.T) {
	path := setupFailover(t)
	require.Equal(t, "OK", run(t, "KEEL.PROMOTE", "1"))
	require.Equal(t, "OK", run(t, "KEEL.FENCE", "2"))
	require.False(t, Writable())
	require.NoError(t, CloseAOF())
	require.NoError(t, LoadTerm(path))
	require.False(t, Writable(), "an observed successor term must not become this node's write authority on restart")
	require.Equal(t, errFenced.Error(), run(t, "SET", "after-restart", "must-refuse"))
}

func TestFailedTermObservationStaysFencedAndRetriesPersistence(t *testing.T) {
	setupFailover(t)
	require.Equal(t, "OK", run(t, "KEEL.PROMOTE", "1"))
	oldSync := termSync
	t.Cleanup(func() { termSync = oldSync })
	termSync = func(*os.File) error { return os.ErrPermission }
	for i := 0; i < 2; i++ {
		require.Contains(t, run(t, "KEEL.FENCE", "5"), "recording term")
		require.Equal(t, uint64(5), CurrentTerm())
		require.Equal(t, uint64(1), failover.persisted)
		require.False(t, Writable())
		require.Equal(t, errFenced.Error(), run(t, "SET", "blocked", "value"))
	}
	termSync = oldSync
	require.Equal(t, "OK", run(t, "KEEL.FENCE", "5"))
	require.Equal(t, uint64(5), failover.persisted)
	require.False(t, Writable(), "persisting an observation does not grant authority")
	require.Equal(t, "OK", run(t, "KEEL.PROMOTE", "6"))
	require.True(t, Writable())
}

func TestReplicaIsNeverReportedWritable(t *testing.T) {
	setupFailover(t)
	config.ReplicaOf = "primary.invalid:6379"
	require.False(t, Writable())
}

func TestNonzeroTermsCannotUseProtocol1(t *testing.T) {
	setupFailover(t)
	oldProtocol, oldFeed := config.ReplicationProtocol, config.ReplicationFeed
	t.Cleanup(func() { config.ReplicationProtocol, config.ReplicationFeed = oldProtocol, oldFeed })
	config.ReplicationProtocol, config.ReplicationFeed = 1, true
	require.NoError(t, InitReplication())
	require.Equal(t, "OK", run(t, "KEEL.PROMOTE", "1"))
	require.Contains(t, run(t, "KEEL.REPL.PULL", "", "0"), "require replication protocol 2")
	replicaReady = true
	require.ErrorContains(t, ApplyReplication(ReplicationFrame{Version: 1}), "require replication protocol 2")
	require.False(t, replicaReady)
}

func TestCurrentTermCanBeReadByTransport(t *testing.T) {
	setupFailover(t)
	ready, stop, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		close(ready)
		for {
			select {
			case <-stop:
				return
			default:
				_ = CurrentTerm()
				runtime.Gosched()
			}
		}
	}()
	<-ready
	for term := uint64(1); term <= 20; term++ {
		if err := observeTerm(term); err != nil {
			close(stop)
			<-done
			t.Fatal(err)
		}
	}
	close(stop)
	<-done
}

func TestProtocol2TermZeroAcceptsLegacyPull(t *testing.T) {
	setupReplicationV2(t)
	old := failover
	failover = failoverState{}
	t.Cleanup(func() { failover = old })
	reply := run(t, "KEEL.REPL.PULL2", "", "0", "", "0")
	require.Contains(t, reply, `"version":2`, "term-zero peers retain the existing protocol-2 request shape")
}
