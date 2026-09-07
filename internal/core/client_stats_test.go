package core

import (
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestINFOClientBuffersHasExplicitScopeAndStableValues(t *testing.T) {
	old := ClientBuffers
	t.Cleanup(func() { ClientBuffers = old })
	ClientBuffers = nil
	require.NotContains(t, string(cmdINFO([]string{"clients"})), "connected_clients")
	ClientBuffers = func() ClientBufferStats {
		return ClientBufferStats{Connected: 3, InputBytes: 10, ReplyBytes: 20, TotalBytes: 30}
	}
	got := string(cmdINFO([]string{"clients"}))
	for _, line := range []string{"connected_clients:3\r\n", "retained_input_bytes:10\r\n", "retained_reply_bytes:20\r\n", "retained_client_bytes:30\r\n"} {
		require.Contains(t, got, line)
	}
	require.False(t, strings.Contains(string(cmdINFO([]string{"memory"})), "connected_clients"))
}
