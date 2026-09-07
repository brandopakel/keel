package core

import (
	"github.com/brandopakel/keel/internal/config"
	"github.com/brandopakel/keel/internal/data_structure"
	"github.com/stretchr/testify/require"
	"strconv"
	"testing"
)

func TestMemoryMaintenancePreservesReplicaKeysAndExpiry(t *testing.T) {
	ResetStores()
	t.Cleanup(ResetStores)
	old := config.ReplicaOf
	t.Cleanup(func() { config.ReplicaOf = old })
	config.ReplicaOf = "127.0.0.1:1"
	for i := 0; i < 4096; i++ {
		key := strconv.Itoa(i)
		dictStore.Put(key, dictStore.NewObj("v"))
		dictStore.SetExpiryAt(key, 1<<60)
	}
	for i := 0; i < 4000; i++ {
		dictStore.ClearExpiry(strconv.Itoa(i))
	}
	before := data_structure.TotalMemUsed()
	worked := 0
	for i := 0; i < 100; i++ {
		work := MaintainMemory()
		require.LessOrEqual(t, work, data_structure.ScanMaxWork)
		worked += work
	}
	require.GreaterOrEqual(t, worked, 4096)
	require.Equal(t, 4096, dictStore.Len())
	require.Equal(t, 96, dictStore.KeysWithExpiry())
	require.Equal(t, before, data_structure.TotalMemUsed(), "physical compaction must not change logical accounting")
	for i := 4000; i < 4096; i++ {
		at, ok := dictStore.GetExpiry(strconv.Itoa(i))
		require.True(t, ok)
		require.EqualValues(t, 1<<60, at)
	}
}
