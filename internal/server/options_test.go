package server

import (
	"testing"

	"github.com/brandopakel/keel/internal/core"
	"github.com/stretchr/testify/require"
)

// withEngineOptions changes the options of the engine the server drives, the
// default engine, for the rest of t, and puts back the ones it found when t
// ends.
func withEngineOptions(t *testing.T, change func(*core.Options)) {
	t.Helper()
	found := core.Configuration()
	o := found
	change(&o)
	require.NoError(t, core.Configure(o))
	t.Cleanup(func() { require.NoError(t, core.Configure(found)) })
}
