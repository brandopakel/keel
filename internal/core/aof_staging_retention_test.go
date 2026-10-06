package core

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func retainedStagingBytes(records [][]string) int {
	bytes := 0
	for _, parts := range records[:cap(records)] {
		bytes += cap(parts) * 16
		for _, part := range parts {
			bytes += len(part)
		}
	}
	return bytes
}

func TestCommittedAOFStagingDoesNotRetainPayloads(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := filepath.Join(t.TempDir(), "store.aof")
	require.NoError(t, e.OpenAOF(path))
	t.Cleanup(func() { require.NoError(t, e.CloseAOF()); e.resetStores() })
	key, value := strings.Repeat("k", 64<<10), strings.Repeat("v", 8<<20)
	require.Equal(t, "OK", runOn(t, e, "SET", key, value, "PX", "600000"))
	staged := retainedStagingBytes(e.aof.staged)
	t.Logf("committed staging still references %d bytes", staged)
	require.Zero(t, staged, "encoded records must release their borrowed field/value references")
	require.Equal(t, int64(1), runOn(t, e, "DEL", key))
	require.Equal(t, "OK", runOn(t, e, "SET", "survivor", "exact"))
	require.NoError(t, e.CloseAOF())
	require.Zero(t, cap(e.aof.buf), "closing a drained log must release its idle buffer")
	for restart := 0; restart < 2; restart++ {
		e.resetStores()
		_, err := e.LoadAOF(path)
		require.NoError(t, err)
		require.Equal(t, int64(0), runOn(t, e, "EXISTS", key))
		require.Equal(t, "exact", runOn(t, e, "GET", "survivor"))
	}
}
