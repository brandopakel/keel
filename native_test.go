package keel_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brandopakel/keel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGetOrSetLoadsOnceForConcurrentMisses: callers that miss the same key at
// once share one load, all answer with what it returned, and it is stored
// with its expiry; a hit loads nothing.
func TestGetOrSetLoadsOnceForConcurrentMisses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := open(t, keel.Options{})
	var loads atomic.Int32
	release := make(chan struct{})
	load := func(context.Context) (string, error) {
		loads.Add(1)
		<-release
		return "loaded", nil
	}
	const callers = 20
	var wg sync.WaitGroup
	results := make([]string, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, err := db.GetOrSet(ctx, "k", load, keel.WithTTL(time.Hour))
			assert.NoError(t, err)
			results[i] = v
		}(i)
	}
	require.Eventually(t, func() bool { return loads.Load() == 1 }, 5*time.Second, time.Millisecond)
	time.Sleep(20 * time.Millisecond) // let the other callers join the load
	close(release)
	wg.Wait()
	assert.EqualValues(t, 1, loads.Load(), "one load for every concurrent miss")
	for _, v := range results {
		assert.Equal(t, "loaded", v)
	}
	ttl, err := db.TTL(ctx, "k")
	require.NoError(t, err)
	assert.Greater(t, ttl, 59*time.Minute)

	v, err := db.GetOrSet(ctx, "k", func(context.Context) (string, error) { t.Error("a hit loads nothing"); return "", nil })
	require.NoError(t, err)
	assert.Equal(t, "loaded", v)
}

// TestGetOrSetFailures: a load that fails stores nothing and answers every
// waiter with its error, and the next call loads again; a panicking load is
// an error, not a crash; a key of another type is ErrWrongType, and nothing
// loads.
func TestGetOrSetFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := open(t, keel.Options{})
	boom := errors.New("backend down")
	_, err := db.GetOrSet(ctx, "k", func(context.Context) (string, error) { return "", boom })
	assert.ErrorIs(t, err, boom)
	n, _ := db.Exists(ctx, "k")
	assert.Zero(t, n, "a failed load stores nothing")
	v, err := db.GetOrSet(ctx, "k", func(context.Context) (string, error) { return "second", nil })
	require.NoError(t, err)
	assert.Equal(t, "second", v)

	_, err = db.GetOrSet(ctx, "p", func(context.Context) (string, error) { panic("oops") })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "oops")

	_, err = db.Do(ctx, "RPUSH", "list", "a")
	require.NoError(t, err)
	_, err = db.GetOrSet(ctx, "list", func(context.Context) (string, error) { t.Error("nothing loads"); return "", nil })
	assert.ErrorIs(t, err, keel.ErrWrongType)
}

// TestGetOrSetCancelledWaiter: a caller whose context is done stops waiting,
// and the load it shares goes on for the others, with a context that is
// never cancelled; a value another writer stores while the load runs is what
// every waiter answers.
func TestGetOrSetCancelledWaiter(t *testing.T) {
	t.Parallel()
	db := open(t, keel.Options{})
	release := make(chan struct{})
	var loadErr atomic.Value
	load := func(ctx context.Context) (string, error) {
		<-release
		if err := ctx.Err(); err != nil {
			loadErr.Store(err)
		}
		return "loaded", nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := db.GetOrSet(ctx, "k", load)
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	other := make(chan string, 1)
	go func() {
		v, _ := db.GetOrSet(context.Background(), "k", load)
		other <- v
	}()
	cancel()
	assert.ErrorIs(t, <-done, context.Canceled)
	// Another writer stores a value while the load still runs.
	require.NoError(t, db.Set(context.Background(), "k", "written"))
	close(release)
	assert.Equal(t, "written", <-other, "SET NX leaves the value another writer stored")
	assert.Nil(t, loadErr.Load(), "the load's context was not cancelled")
	v, _ := db.Get(context.Background(), "k")
	assert.Equal(t, "written", v)
}

// TestGetOrSetIsLoggedAsItsSet: what GetOrSet stores is logged as the SET and
// PEXPIREAT it performs, and replays.
func TestGetOrSetIsLoggedAsItsSet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "log.aof")
	db, err := keel.Open(ctx, persistent(t, path, keel.FsyncAlways))
	require.NoError(t, err)
	at := time.UnixMilli(4102444800123)
	_, err = db.GetOrSet(ctx, "k", func(context.Context) (string, error) { return "v", nil }, keel.WithExpireAt(at))
	require.NoError(t, err)
	require.NoError(t, db.Close())
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(body), "*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$1\r\nv\r\n*3\r\n$9\r\nPEXPIREAT\r\n$1\r\nk\r\n$13\r\n4102444800123\r\n")
	db = open(t, persistent(t, path, keel.FsyncAlways))
	when, err := db.ExpireTime(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, at, when)
}

// TestCompareAndSwapAndDeleteIf: SET IFEQ and DELEX IFEQ, as methods.
func TestCompareAndSwapAndDeleteIf(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := open(t, keel.Options{})
	require.NoError(t, db.Set(ctx, "k", "a", keel.WithTTL(time.Hour)))
	swapped, err := db.CompareAndSwap(ctx, "k", "b", "c")
	require.NoError(t, err)
	assert.False(t, swapped, "another string is left")
	swapped, _ = db.CompareAndSwap(ctx, "k", "a", "b", keel.WithKeepTTL())
	assert.True(t, swapped)
	v, _ := db.Get(ctx, "k")
	assert.Equal(t, "b", v)
	ttl, _ := db.TTL(ctx, "k")
	assert.Greater(t, ttl, 59*time.Minute, "KEEPTTL kept the expiry")
	swapped, _ = db.CompareAndSwap(ctx, "k", "b", "c")
	assert.True(t, swapped)
	ttl, _ = db.TTL(ctx, "k")
	assert.Equal(t, keel.NoTTL, ttl, "a swap clears the expiry, as SET does")
	swapped, _ = db.CompareAndSwap(ctx, "missing", "", "x")
	assert.False(t, swapped, "a key that holds nothing is left")

	_, err = db.Do(ctx, "RPUSH", "list", "a")
	require.NoError(t, err)
	_, err = db.CompareAndSwap(ctx, "list", "a", "b")
	assert.ErrorIs(t, err, keel.ErrWrongType)

	deleted, err := db.DeleteIf(ctx, "k", "x")
	require.NoError(t, err)
	assert.False(t, deleted)
	deleted, _ = db.DeleteIf(ctx, "k", "c")
	assert.True(t, deleted)
	n, _ := db.Exists(ctx, "k")
	assert.Zero(t, n)
	deleted, _ = db.DeleteIf(ctx, "k", "c")
	assert.False(t, deleted)
	_, err = db.DeleteIf(ctx, "list", "a")
	assert.ErrorIs(t, err, keel.ErrWrongType)
	assert.EqualError(t, err, "ERR Key should be of string type if conditions are specified", "DELEX's own words")
}

// infoFields is INFO's fields, by name.
func infoFields(t *testing.T, db *keel.DB) map[string]string {
	t.Helper()
	reply, err := db.Do(context.Background(), "INFO")
	require.NoError(t, err)
	fields := map[string]string{}
	for _, line := range strings.Split(reply.(string), "\r\n") {
		if name, value, ok := strings.Cut(line, ":"); ok {
			fields[name] = value
		}
	}
	return fields
}

// TestStatsAgreesWithINFO: Stats reads the counters INFO renders, so every
// field it shares with INFO says the same.
func TestStatsAgreesWithINFO(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, o := range []keel.Options{{MaxMemory: 64 << 20}, persistent(t, filepath.Join(t.TempDir(), "log.aof"), keel.FsyncEverySec)} {
		db := open(t, o)
		for i := 0; i < 50; i++ {
			require.NoError(t, db.Set(ctx, "k"+strconv.Itoa(i), "v"))
			db.Get(ctx, "k"+strconv.Itoa(i))
			db.Get(ctx, "missing")
		}
		db.Expire(ctx, "k0", time.Hour)
		db.Incr(ctx, "k1")
		s, err := db.Stats(ctx)
		require.NoError(t, err)
		info := infoFields(t, db)
		for name, got := range map[string]any{
			"total_commands_processed": s.Commands, "total_error_replies": s.ErrorReplies,
			"keyspace_hits": s.KeyspaceHits, "keyspace_misses": s.KeyspaceMisses,
			"expired_keys": s.ExpiredKeys, "evicted_keys": s.EvictedKeys,
			"used_memory": s.UsedMemory, "used_memory_peak": s.PeakMemory, "maxmemory": s.MaxMemory,
		} {
			assert.Equal(t, info[name], fmt.Sprint(got), name)
		}
		assert.Equal(t, fmt.Sprintf("keys=%d,expires=%d", s.Keys, s.KeysWithExpiry), info["db0"])
		if s.Persistence != nil {
			assert.Equal(t, "1", info["aof_enabled"])
			assert.Equal(t, fmt.Sprint(s.Persistence.Size), info["aof_current_size"])
			assert.Equal(t, fmt.Sprint(s.Persistence.BaseSize), info["aof_base_size"])
			assert.Equal(t, fmt.Sprint(s.Persistence.Rewrites), info["aof_rewrites"])
			assert.Equal(t, "ok", info["aof_last_write_status"])
		} else {
			assert.Equal(t, "0", info["aof_enabled"])
		}
		s2, _ := db.Stats(ctx)
		assert.Equal(t, s.Commands+1, s2.Commands, "INFO counted itself once it ran; Stats counts as nothing")
	}
}
