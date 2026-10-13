package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/brandopakel/keel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRun: each client gets its limit a window, and a new window resets it.
func TestRun(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	require.NoError(t, run(context.Background(), &out, 200*time.Millisecond))
	assert.Equal(t, "ada: 5 of 8 requests allowed\ngrace: 3 of 3 requests allowed\nada, in the next window: allowed true\n", out.String())
}

// TestWindowStartsWithTheFirstRequest: the window's expiry is set by the
// first request alone, so later requests do not stretch it, and the counter
// is gone when it ends.
func TestWindowStartsWithTheFirstRequest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := keel.Open(ctx, keel.Options{})
	require.NoError(t, err)
	defer db.Close()
	limiter := &Limiter{db: db, limit: 2, window: time.Hour}

	_, err = limiter.Allow(ctx, "ada")
	require.NoError(t, err)
	first, err := db.ExpireTime(ctx, "ratelimit:ada")
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)
	_, err = limiter.Allow(ctx, "ada")
	require.NoError(t, err)
	again, err := db.ExpireTime(ctx, "ratelimit:ada")
	require.NoError(t, err)
	assert.Equal(t, first, again, "EXPIRE NX left the window's end where the first request put it")
	ok, err := limiter.Allow(ctx, "ada")
	require.NoError(t, err)
	assert.False(t, ok, "the third request of a window of two")
}
