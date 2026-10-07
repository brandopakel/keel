package core

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// eventually holds e's lock to ask done, until it says yes, failing the test
// after a deadline that only a hang would reach: nothing here is timed.
func eventually(t *testing.T, e *Engine, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var ok bool
		holding(e, func() { ok = done() })
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the maintenance goroutine never got there: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestOpenRefusesWhatOnlyTheServerDrives: AsyncAppend and a replication role
// need the server's loop and transport, so Open refuses them before it
// touches anything.
func TestOpenRefusesWhatOnlyTheServerDrives(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "keel.aof")
	for _, c := range []struct {
		o    Options
		want error
	}{
		{Options{AppendOnly: true, AppendFilename: path, AsyncAppend: true}, errOpenAsyncAppend},
		{Options{AppendOnly: true, AppendFilename: path, ReplicaOf: "127.0.0.1:6379"}, errOpenReplication},
		{Options{AppendOnly: true, AppendFilename: path, ReplicationFeed: true}, errOpenReplication},
	} {
		e, err := Open(context.Background(), c.o)
		require.ErrorIs(t, err, c.want)
		assert.Nil(t, e)
	}
	assert.Empty(t, filesIn(t, dir))
}

// TestKeysExpireWithNobodyReadingThem: the maintenance goroutine runs the
// expiry cycle with no call to prompt it, so keys whose TTL has passed are
// reaped by it rather than by a read, and counted as active expiry counts.
func TestKeysExpireWithNobodyReadingThem(t *testing.T) {
	t.Parallel()
	e := openTestEngine(t, Options{})
	const keys = 100
	holding(e, func() {
		for i := range keys {
			runOn(t, e, "SET", fmt.Sprintf("k%d", i), "v", "PX", "1")
		}
	})
	eventually(t, e, "every key expired", func() bool { return e.expiredKeys == keys })
	holding(e, func() { assert.Zero(t, e.space.TotalKeys()) })
}

// TestAnEverysecLogIsSyncedWithNoCallAfterTheWrite: the maintenance
// goroutine writes the buffer within a cycle and syncs it within a tick of the
// second, with nothing else touching the engine after the write.
func TestAnEverysecLogIsSyncedWithNoCallAfterTheWrite(t *testing.T) {
	t.Parallel()
	e := openTestEngine(t, Options{AppendOnly: true, AppendFilename: filepath.Join(t.TempDir(), "keel.aof"), Fsync: FsyncEverySec})
	var end uint64
	holding(e, func() {
		runOn(t, e, "SET", "k", "v")
		end = e.AppendOffset()
	})
	require.NotZero(t, end)
	eventually(t, e, "the write written", func() bool { _, written, _, _ := e.AOFPositions(); return written >= end })
	eventually(t, e, "the write synced", func() bool { _, _, synced, _ := e.AOFPositions(); return synced >= end })
}

// TestABGREWRITEAOFFinishesWithNoFurtherCalls: a rewrite advances a slice a
// cycle, and the maintenance goroutine runs the next cycle at once while it
// has slices left, so a rewrite started by one call finishes with no other,
// and the rewritten log replays to the same keyspace.
func TestABGREWRITEAOFFinishesWithNoFurtherCalls(t *testing.T) {
	t.Parallel()
	o := Options{AppendOnly: true, AppendFilename: filepath.Join(t.TempDir(), "keel.aof")}
	e := openTestEngine(t, o)
	const keys = 5000
	holding(e, func() {
		for i := range keys {
			runOn(t, e, "SET", fmt.Sprintf("k%d", i), strconv.Itoa(i))
			runOn(t, e, "INCR", fmt.Sprintf("k%d", i))
		}
		assert.Equal(t, "Background append only file rewriting started", runOn(t, e, "BGREWRITEAOF"))
	})
	eventually(t, e, "the rewrite finished", func() bool { return !e.RewriteActive() && e.aof.rewrites == 1 })
	require.NoError(t, e.Close())

	again := openTestEngine(t, o)
	holding(again, func() {
		assert.Equal(t, int64(keys), runOn(t, again, "DBSIZE"))
		assert.Equal(t, strconv.Itoa(keys), runOn(t, again, "GET", fmt.Sprintf("k%d", keys-1)))
	})
}

// TestCloseStopsTheMaintenanceGoroutine: Close returns only once the
// goroutine has returned, and a worker that wakes it afterwards finds nothing
// to wake. An engine nobody drives but its maker has none.
func TestCloseStopsTheMaintenanceGoroutine(t *testing.T) {
	t.Parallel()
	e := openTestEngine(t, Options{AppendOnly: true, AppendFilename: filepath.Join(t.TempDir(), "keel.aof")})
	d := e.driver
	require.NotNil(t, d)
	require.NoError(t, e.Close())
	select {
	case <-d.done:
	default:
		t.Fatal("Close returned before the maintenance goroutine did")
	}
	d.wake()
	d.wake()

	assert.Nil(t, newTestEngine(t, Options{}).driver, "NewEngine starts no goroutine: its maker drives it")
}

// TestTheMaintenanceGoroutineSharesTheEngine: goroutines that hold the
// engine's lock to run commands, and the maintenance goroutine's cycles of
// expiry, flushes and syncs, take turns; under -race, a cycle that touched
// the engine without the lock would be reported. Each goroutine writes keys
// of its own, and the log, reopened, holds what each wrote.
func TestTheMaintenanceGoroutineSharesTheEngine(t *testing.T) {
	t.Parallel()
	o := Options{AppendOnly: true, AppendFilename: filepath.Join(t.TempDir(), "keel.aof"), Fsync: FsyncEverySec}
	e := openTestEngine(t, o)
	const writers, rounds = 4, 300
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range rounds {
				holding(e, func() {
					runOn(t, e, "INCR", fmt.Sprintf("w%d:count", w))
					runOn(t, e, "SET", fmt.Sprintf("w%d:brief%d", w, i), "v", "PX", "1")
					runOn(t, e, "HSET", fmt.Sprintf("w%d:hash", w), strconv.Itoa(i), "v")
				})
			}
		}()
	}
	wg.Wait()
	require.NoError(t, e.Close())

	again := openTestEngine(t, o)
	holding(again, func() {
		for w := range writers {
			assert.Equal(t, strconv.Itoa(rounds), runOn(t, again, "GET", fmt.Sprintf("w%d:count", w)))
			assert.Equal(t, int64(rounds), runOn(t, again, "HLEN", fmt.Sprintf("w%d:hash", w)))
		}
	})
}
