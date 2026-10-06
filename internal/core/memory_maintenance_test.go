package core

import (
	"github.com/brandopakel/keel/internal/data_structure"
	"github.com/stretchr/testify/require"
	"strconv"
	"testing"
)

func TestMemoryMaintenancePreservesReplicaKeysAndExpiry(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{ReplicaOf: "127.0.0.1:1"})
	for i := 0; i < 4096; i++ {
		key := strconv.Itoa(i)
		e.dictStore.Put(key, e.dictStore.NewObj("v"))
		e.dictStore.SetExpiryAt(key, 1<<60)
	}
	for i := 0; i < 4000; i++ {
		e.dictStore.ClearExpiry(strconv.Itoa(i))
	}
	before := e.space.TotalMemUsed()
	worked := 0
	for i := 0; i < 100; i++ {
		work := e.MaintainMemory()
		require.LessOrEqual(t, work, data_structure.ScanMaxWork)
		worked += work
	}
	require.GreaterOrEqual(t, worked, 4096)
	require.Equal(t, 4096, e.dictStore.Len())
	require.Equal(t, 96, e.dictStore.KeysWithExpiry())
	require.Equal(t, before, e.space.TotalMemUsed(), "physical compaction must not change logical accounting")
	for i := 4000; i < 4096; i++ {
		at, ok := e.dictStore.GetExpiry(strconv.Itoa(i))
		require.True(t, ok)
		require.EqualValues(t, 1<<60, at)
	}
}
