package server

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// infoStats reads INFO stats and errorstats through c, by field name.
func infoStats(t *testing.T, c *client) map[string]string {
	t.Helper()
	reply := runOnce(t, c, sent("info", "stats", "errorstats"))
	fields := map[string]string{}
	for _, line := range strings.Split(reply, "\r\n") {
		if name, value, ok := strings.Cut(line, ":"); ok && !strings.HasPrefix(name, "$") && !strings.HasPrefix(name, "=") {
			fields[name] = value
		}
	}
	return fields
}

// TestTransportCountsWhatItAnswersAsRedisDoes: the commands the transport
// answers itself count as they run, its refusals do not, and every error it
// writes counts under its prefix, inside EXEC's reply too; what the engine
// answers is counted there, once.
func TestTransportCountsWhatItAnswersAsRedisDoes(t *testing.T) {
	c := clientWithPassword(t, "secret")
	runOnce(t, c, sent("get", "k"))                 // NOAUTH: refused
	runOnce(t, c, sent("nosuch"))                   // unknown: refused
	runOnce(t, c, sent("auth", "wrong"))            // runs, WRONGPASS
	runOnce(t, c, sent("auth", "secret"))           // runs
	runOnce(t, c, sent("client", "setname", "a b")) // runs, an ERR
	runOnce(t, c, sent("client", "id"))             // runs
	runOnce(t, c, sent("set", "k", "v"))            // the engine's
	runOnce(t, c, sent("multi"), sent("client", "id"), sent("exec"))
	stats := infoStats(t, c)
	// AUTH twice, CLIENT twice, SET, MULTI, the CLIENT ID EXEC ran, and EXEC.
	assert.Equal(t, "8", stats["total_commands_processed"])
	assert.Equal(t, "4", stats["total_error_replies"])
	assert.Equal(t, "count=1", stats["errorstat_NOAUTH"])
	assert.Equal(t, "count=1", stats["errorstat_WRONGPASS"])
	assert.Equal(t, "count=2", stats["errorstat_ERR"])

	quit := clientWithPassword(t, "")
	runOnce(t, quit, sent("quit"))
	assert.True(t, quit.closeAfterWrite)
	assert.Equal(t, "1", infoStats(t, clientWithEngine(quit))["total_commands_processed"], "QUIT runs")
}

// clientWithEngine is another connection to c's engine.
func clientWithEngine(c *client) *client { return &client{fd: -1, engine: c.engine} }

// TestResetStatKeepsClientIdsGrowing: CONFIG RESETSTAT starts the connection
// counts again, and the next client still gets an id no client had before.
func TestResetStatKeepsClientIdsGrowing(t *testing.T) {
	received, base, rejected := connectionsReceived, connectionsReceivedBase, connectionsRejected
	t.Cleanup(func() { connectionsReceived, connectionsReceivedBase, connectionsRejected = received, base, rejected })
	connectionsReceived, connectionsReceivedBase, connectionsRejected = 7, 0, 2
	resetConnectionStats()
	assert.Equal(t, uint64(0), connectionsReceived-connectionsReceivedBase)
	assert.Zero(t, connectionsRejected)
	assert.Equal(t, uint64(7), connectionsReceived, "ids go on from where they were")
}
