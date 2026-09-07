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
	ResetStores()
	path := filepath.Join(t.TempDir(), "store.aof")
	require.NoError(t, OpenAOF(path))
	t.Cleanup(func() { require.NoError(t, CloseAOF()); ResetStores() })
	members := []string{"set"}
	for i := 0; i < 8192; i++ {
		members = append(members, strconv.Itoa(i))
	}
	run(t, "SADD", members...)
	run(t, "SREM", append([]string{"set"}, members[1025:]...)...)
	require.NoError(t, FlushAOF())
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	s, ok := setStore.Peek("set")
	require.True(t, ok)
	order := s.Members()
	for i := 0; i < 100; i++ {
		require.LessOrEqual(t, MaintainMemory(), data_structure.ScanMaxWork)
	}
	require.Equal(t, order, s.Members())
	require.NoError(t, FlushAOF())
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, after, "physical maintenance emits no logical records")
	require.NoError(t, CloseAOF())
	ResetStores()
	_, err = LoadAOF(path)
	require.NoError(t, err)
	s, ok = setStore.Peek("set")
	require.True(t, ok)
	require.ElementsMatch(t, order, s.Members())
}
