package data_structure

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestKeyspaceVisitStopsAndResumesWithoutWalkingRemainder(t *testing.T) {
	old := keyspaces
	t.Cleanup(func() { keyspaces = old })
	keyspaces = make([]Keyspace, 10000)
	calls := 0
	next := VisitKeyspacesFrom(37, func(Keyspace) bool { calls++; return false })
	require.Equal(t, 1, calls)
	require.Equal(t, 38, next)
	calls = 0
	next = VisitKeyspacesFrom(len(keyspaces)-1, func(Keyspace) bool { calls++; return calls < 2 })
	require.Equal(t, 2, calls)
	require.Equal(t, 1, next)
	calls = 0
	next = VisitKeyspacesFrom(10, func(Keyspace) bool { calls++; return true })
	require.Equal(t, len(keyspaces), calls)
	require.Equal(t, 11, next)
	keyspaces = nil
	require.Zero(t, VisitKeyspacesFrom(10, func(Keyspace) bool { t.Fatal("visited empty registry"); return true }))
}
