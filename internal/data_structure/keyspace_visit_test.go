package data_structure

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestKeyspaceVisitStopsAndResumesWithoutWalkingRemainder(t *testing.T) {
	t.Parallel()
	s := NewSpace(DefaultLimits())
	s.keyspaces = make([]Keyspace, 10000)
	calls := 0
	next := s.VisitKeyspacesFrom(37, func(Keyspace) bool { calls++; return false })
	require.Equal(t, 1, calls)
	require.Equal(t, 38, next)
	calls = 0
	next = s.VisitKeyspacesFrom(len(s.keyspaces)-1, func(Keyspace) bool { calls++; return calls < 2 })
	require.Equal(t, 2, calls)
	require.Equal(t, 1, next)
	calls = 0
	next = s.VisitKeyspacesFrom(10, func(Keyspace) bool { calls++; return true })
	require.Equal(t, len(s.keyspaces), calls)
	require.Equal(t, 11, next)
	s.keyspaces = nil
	require.Zero(t, s.VisitKeyspacesFrom(10, func(Keyspace) bool { t.Fatal("visited empty registry"); return true }))
}
