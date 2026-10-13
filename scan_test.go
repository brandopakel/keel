package keel_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brandopakel/keel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestScanSeesEveryKeyPresentThroughout: under concurrent writes - keys
// added and deleted while the walk goes on, the keyspace growing past the
// size it started at - every key present for the whole walk is yielded at
// least once, as Redis's SCAN guarantees.
func TestScanSeesEveryKeyPresentThroughout(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := open(t, keel.Options{})
	const stable = 5000
	for i := range stable {
		require.NoError(t, db.Set(ctx, "stable:"+strconv.Itoa(i), "v"))
	}
	for _, count := range []int{0, 1, 100} {
		var stop atomic.Bool
		var wg sync.WaitGroup
		for w := range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				// Bounded, so the walk ends however the goroutines are
				// scheduled: the keyspace grows by up to 4000 keys.
				for i := 0; i < 2000 && !stop.Load(); i++ {
					key := fmt.Sprintf("churn:%d:%d:%d", count, w, i)
					assert.NoError(t, db.Set(ctx, key, "v"))
					if i%2 == 0 {
						_, err := db.Del(ctx, key)
						assert.NoError(t, err)
					}
				}
			}()
		}
		seen := map[string]bool{}
		for key, err := range db.Scan(ctx, keel.ScanOptions{Count: count}) {
			require.NoError(t, err)
			seen[key] = true
		}
		stop.Store(true)
		wg.Wait()
		for i := range stable {
			require.True(t, seen["stable:"+strconv.Itoa(i)], "count %d: stable:%d was never yielded", count, i)
		}
	}
}

// TestScanBodyCallsBackIntoTheDB: the loop's body runs without the lock, so
// it can read, write and delete through the DB - the keys it walks
// included - and others' calls run between steps.
func TestScanBodyCallsBackIntoTheDB(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := open(t, keel.Options{})
	for i := range 500 {
		require.NoError(t, db.Set(ctx, "k"+strconv.Itoa(i), strconv.Itoa(i)))
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		walked := 0
		for key, err := range db.Scan(ctx, keel.ScanOptions{Match: "k*"}) {
			if !assert.NoError(t, err) {
				return
			}
			v, err := db.Get(ctx, key)
			if err == nil {
				assert.NoError(t, db.Set(ctx, "copy:"+key, v))
				_, err = db.Del(ctx, key)
				assert.NoError(t, err)
			}
			// Another caller's call runs while the body waits on it.
			other := make(chan error, 1)
			go func() { _, err := db.Incr(ctx, "calls"); other <- err }()
			assert.NoError(t, <-other)
			walked++
		}
		assert.GreaterOrEqual(t, walked, 500)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the walk deadlocked")
	}
	n, err := db.Exists(ctx, "k0", "k499")
	require.NoError(t, err)
	assert.Zero(t, n, "the body deleted every key it walked")
	v, err := db.Get(ctx, "copy:k499")
	require.NoError(t, err)
	assert.Equal(t, "499", v)

	// Stopping early stops the walk.
	steps := 0
	for range db.Scan(ctx, keel.ScanOptions{Count: 1}) {
		steps++
		break
	}
	assert.Equal(t, 1, steps)
}

// TestScanFiltersAsSCANDoes: Match and Type keep what SCAN's MATCH and TYPE
// keep, and a refused option or a done context ends the walk with its error.
func TestScanFiltersAsSCANDoes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := open(t, keel.Options{})
	for i := range 50 {
		require.NoError(t, db.Set(ctx, fmt.Sprintf("user:%d", i), "v"))
		require.NoError(t, db.Set(ctx, fmt.Sprintf("session:%d", i), "v"))
		_, err := db.Do(ctx, "RPUSH", fmt.Sprintf("user:list:%d", i), "a")
		require.NoError(t, err)
	}
	require.NoError(t, db.Set(ctx, "user:gone", "v", keel.WithTTL(time.Millisecond)))
	time.Sleep(5 * time.Millisecond)

	// Each walk's keys, sorted and once each: a walk may yield a key twice,
	// as SCAN may.
	distinct := func(seen map[string]bool) []string {
		keys := make([]string, 0, len(seen))
		for key := range seen {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		return keys
	}
	scan := func(o keel.ScanOptions) []string {
		seen := map[string]bool{}
		for key, err := range db.Scan(ctx, o) {
			require.NoError(t, err)
			seen[key] = true
		}
		return distinct(seen)
	}
	viaSCAN := func(args ...string) []string {
		seen := map[string]bool{}
		cursor := "0"
		for {
			reply, err := db.Do(ctx, append([]string{"SCAN", cursor}, args...)...)
			require.NoError(t, err)
			step := reply.([]any)
			for _, key := range step[1].([]any) {
				seen[key.(string)] = true
			}
			if cursor = step[0].(string); cursor == "0" {
				break
			}
		}
		return distinct(seen)
	}
	assert.Equal(t, viaSCAN(), scan(keel.ScanOptions{}))
	assert.Len(t, scan(keel.ScanOptions{}), 150, "an expired key is not yielded")
	assert.Equal(t, viaSCAN("MATCH", "user:*"), scan(keel.ScanOptions{Match: "user:*"}))
	assert.Equal(t, viaSCAN("TYPE", "list"), scan(keel.ScanOptions{Type: "list"}))
	assert.Len(t, scan(keel.ScanOptions{Type: "list"}), 50)
	assert.Equal(t, viaSCAN("MATCH", "user:[0-4]", "TYPE", "string", "COUNT", "3"),
		scan(keel.ScanOptions{Match: "user:[0-4]", Type: "string", Count: 3}))

	for _, o := range []keel.ScanOptions{{Count: -1}, {Count: 1 << 30}} {
		var errs []error
		for key, err := range db.Scan(ctx, o) {
			assert.Empty(t, key)
			errs = append(errs, err)
		}
		require.Len(t, errs, 1)
		assert.EqualError(t, errs[0], "ERR syntax error")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	for _, err := range db.Scan(cancelled, keel.ScanOptions{}) {
		assert.ErrorIs(t, err, context.Canceled)
	}
}

// TestMGetMSetAndBytes: MGet answers a map of the keys that hold strings;
// MSet is one MSET, logged with its keys sorted; the byte forms copy.
func TestMGetMSetAndBytes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "log.aof")
	db, err := keel.Open(ctx, persistent(t, path, keel.FsyncAlways))
	require.NoError(t, err)
	require.NoError(t, db.MSet(ctx, map[string]string{"c": "3", "a": "1", "b": "2"}))
	require.NoError(t, db.MSet(ctx, nil))
	_, err = db.Do(ctx, "RPUSH", "list", "x")
	require.NoError(t, err)

	stats, err := db.Stats(ctx)
	require.NoError(t, err)
	m, err := db.MGet(ctx, "a", "b", "c", "list", "nobody")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"a": "1", "b": "2", "c": "3"}, m)
	typed, err := db.Stats(ctx)
	require.NoError(t, err)
	_, err = db.Do(ctx, "MGET", "a", "b", "c", "list", "nobody")
	require.NoError(t, err)
	command, err := db.Stats(ctx)
	require.NoError(t, err)
	assert.Equal(t, command.KeyspaceHits-typed.KeyspaceHits, typed.KeyspaceHits-stats.KeyspaceHits,
		"a key of another type is a hit, as MGET counts it")
	assert.Equal(t, command.KeyspaceMisses-typed.KeyspaceMisses, typed.KeyspaceMisses-stats.KeyspaceMisses)
	m, err = db.MGet(ctx)
	require.NoError(t, err)
	assert.Empty(t, m)

	value := []byte("bytes")
	require.NoError(t, db.SetBytes(ctx, "raw", value, keel.WithTTL(time.Hour)))
	value[0] = 'X'
	got, err := db.GetBytes(ctx, "raw")
	require.NoError(t, err)
	assert.Equal(t, []byte("bytes"), got, "SetBytes copied its value")
	got[0] = 'Y'
	again, err := db.GetBytes(ctx, "raw")
	require.NoError(t, err)
	assert.Equal(t, []byte("bytes"), again, "GetBytes answered a copy")
	_, err = db.GetBytes(ctx, "nobody")
	assert.ErrorIs(t, err, keel.ErrNotFound)
	require.NoError(t, db.Close())

	body, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(body), records([]string{"MSET", "a", "1", "b", "2", "c", "3"}))
}
