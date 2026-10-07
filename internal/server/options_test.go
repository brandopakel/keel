package server

import (
	"testing"

	"github.com/brandopakel/keel/internal/core"
	"github.com/stretchr/testify/require"
)

// newTestEngine returns an engine of t's own, held to o, for the server code
// a test drives to run on, as cmd/keel makes the server's. Its log is closed
// when t ends, before t's temporary directories are removed.
func newTestEngine(t *testing.T, o core.Options) *core.Engine {
	t.Helper()
	t.TempDir()
	e, err := core.NewEngine(o)
	require.NoError(t, err)
	t.Cleanup(func() { _ = e.CloseAOF() })
	return e
}
