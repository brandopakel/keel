package core

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/brandopakel/keel/internal/data_structure"
	"github.com/stretchr/testify/require"
)

func TestZSetMaintenancePreservesRankScoresAndPersistence(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := filepath.Join(t.TempDir(), "store.aof")
	require.NoError(t, e.OpenAOF(path))
	t.Cleanup(func() { require.NoError(t, e.CloseAOF()); e.resetStores() })
	args := []string{"ranking"}
	for i := 0; i < 8192; i++ {
		args = append(args, strconv.Itoa(i%100), strconv.Itoa(i))
	}
	runOn(t, e, "ZADD", args...)
	remove := []string{"ranking"}
	for i := 1024; i < 8192; i++ {
		remove = append(remove, strconv.Itoa(i))
	}
	runOn(t, e, "ZREM", remove...)
	require.NoError(t, e.FlushAOF())
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	z, ok := e.zsetStore.Peek("ranking")
	require.True(t, ok)
	expected := runOn(t, e, "ZRANGE", "ranking", "0", "-1", "WITHSCORES")
	require.Equal(t, 1, z.CompactIndex(1), "fixture requires a pending rebuild")
	for i := 0; i < 100; i++ {
		require.LessOrEqual(t, e.MaintainMemory(), data_structure.ScanMaxWork)
	}
	require.Zero(t, z.CompactIndex(1), "serving maintenance must complete the rebuild")
	require.Equal(t, expected, runOn(t, e, "ZRANGE", "ranking", "0", "-1", "WITHSCORES"))
	require.NoError(t, e.FlushAOF())
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, after, "physical maintenance must not emit logical records")
	require.NoError(t, e.CloseAOF())
	e.resetStores()
	_, err = e.LoadAOF(path)
	require.NoError(t, err)
	require.Equal(t, expected, runOn(t, e, "ZRANGE", "ranking", "0", "-1", "WITHSCORES"))
}
