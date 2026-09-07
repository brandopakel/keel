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
	ResetStores()
	path := filepath.Join(t.TempDir(), "store.aof")
	require.NoError(t, OpenAOF(path))
	t.Cleanup(func() { require.NoError(t, CloseAOF()); ResetStores() })
	key, value := strings.Repeat("k", 64<<10), strings.Repeat("v", 8<<20)
	require.Equal(t, "OK", run(t, "SET", key, value, "PX", "600000"))
	staged := retainedStagingBytes(aof.staged)
	t.Logf("committed staging still references %d bytes", staged)
	require.Zero(t, staged, "encoded records must release their borrowed field/value references")
	require.Equal(t, int64(1), run(t, "DEL", key))
	require.Zero(t, retainedStagingBytes(aof.extra), "committed removals must release key references")
	require.Equal(t, "OK", run(t, "SET", "survivor", "exact"))
	require.NoError(t, CloseAOF())
	require.Zero(t, cap(aof.buf), "closing a drained log must release its idle buffer")
	for restart := 0; restart < 2; restart++ {
		ResetStores()
		_, err := LoadAOF(path)
		require.NoError(t, err)
		require.Equal(t, int64(0), run(t, "EXISTS", key))
		require.Equal(t, "exact", run(t, "GET", "survivor"))
	}
}
