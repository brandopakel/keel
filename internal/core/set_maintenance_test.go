package core

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/brandopakel/keel/internal/data_structure"
	"github.com/stretchr/testify/require"
)

func TestSetMaintenancePreservesOrderAndPersistence(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := filepath.Join(t.TempDir(), "store.aof")
	require.NoError(t, e.OpenAOF(path))
	t.Cleanup(func() { require.NoError(t, e.CloseAOF()); e.resetStores() })
	members := []string{"set"}
	for i := 0; i < 8192; i++ {
		members = append(members, strconv.Itoa(i))
	}
	runOn(t, e, "SADD", members...)
	runOn(t, e, "SREM", append([]string{"set"}, members[1025:]...)...)
	require.NoError(t, e.FlushAOF())
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	s, ok := e.setStore.Peek("set")
	require.True(t, ok)
	order := s.Members()
	require.Equal(t, 1, s.CompactIndex(1), "fixture must have a pending membership rebuild")
	for i := 0; i < 100; i++ {
		require.LessOrEqual(t, e.MaintainMemory(), data_structure.ScanMaxWork)
	}
	require.Zero(t, s.CompactIndex(1), "scheduled maintenance must finish the pending rebuild")
	require.Equal(t, order, s.Members())
	require.NoError(t, e.FlushAOF())
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, after, "physical maintenance emits no logical records")
	require.NoError(t, e.CloseAOF())
	e.resetStores()
	_, err = e.LoadAOF(path)
	require.NoError(t, err)
	s, ok = e.setStore.Peek("set")
	require.True(t, ok)
	// Set reply order is unspecified across replay. Maintenance itself must
	// preserve the live sequence (asserted above); replay preserves membership.
	require.ElementsMatch(t, order, s.Members())
}
