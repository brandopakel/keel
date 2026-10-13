package main

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCache: concurrent misses of a user share its load, so the first round
// loads each user about once and the second, served from the cache, not at
// all; and with a log, a second run starts warm.
func TestCache(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	aof := filepath.Join(t.TempDir(), "cache.aof")

	var out bytes.Buffer
	require.NoError(t, run(ctx, &out, aof))
	var first, second int
	_, err := fmt.Sscanf(out.String(), "round 1: 100 requests for 4 users, %d loads\nround 2: 100 requests for 4 users, %d loads\n", &first, &second)
	require.NoError(t, err, out.String())
	// A request that misses just as another's load is stored starts a load
	// of its own, which stores nothing; it cannot happen for every request.
	assert.GreaterOrEqual(t, first, 4)
	assert.Less(t, first, 50)
	assert.Zero(t, second, "the second round is served from the cache")
	assert.Contains(t, out.String(), "cache holds 4 keys, 4 expiring")

	out.Reset()
	require.NoError(t, run(ctx, &out, aof))
	assert.Contains(t, out.String(), "round 1: 100 requests for 4 users, 0 loads", "the log keeps the cache warm")
}
