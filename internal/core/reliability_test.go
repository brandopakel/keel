package core

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestUntrustedSizingAndRestore(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	for _, parts := range [][]string{
		{"MORRIS.INITBYDIM", "m", "4294967295", "4294967295"},
		{"MORRIS.INITBYPROB", "m", "NaN", "0.1"},
		{"MORRIS.INITBYPROB", "m", "1e-100", "0.1"},
		{"CF.RESERVE", "c", "18446744073709551615"},
	} {
		require.Equal(t, byte('-'), rawReplyOn(t, e, parts[0], parts[1:]...)[0])
	}
	payload := []byte{dumpTagCuckoo}
	for _, n := range []uint64{1 << 61, 0, 0, 0, 1, 1 << 63} {
		payload = binary.LittleEndian.AppendUint64(payload, n)
	}
	runOn(t, e, "SET", "keep", "value")
	require.Error(t, e.restoreKey("keep", payload))
	require.Equal(t, "value", runOn(t, e, "GET", "keep"))
	require.Zero(t, e.space.TotalKeys()-1)
}

func TestFailedMorrisBatchIsAtomicAcrossRestart(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := withAOFOn(t, e, func() {
		runOn(t, e, "MORRIS.INITBYDIM", "m", "100", "3")
		before, _ := e.dumpKey("m")
		require.Equal(t, byte('-'), rawReplyOn(t, e, "MORRIS.INCRBY", "m", "a", "1", "b", "invalid")[0])
		after, _ := e.dumpKey("m")
		require.Equal(t, before, after)
	})
	restartOn(t, e, path)
	require.Equal(t, []interface{}{"0"}, runOn(t, e, "MORRIS.QUERY", "m", "a"))
}

func TestAccountingMutationAndExpiry(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "SET", "n", "9")
	runOn(t, e, "INCR", "n")
	runOn(t, e, "DEL", "n")
	require.Zero(t, e.space.TotalMemUsed())
	defer e.resetStores()
	for _, budget := range []uint64{100, 120, 150} {
		e.resetStores()
		reconfigure(t, e, func(o *Options) { o.MaxMemory = budget })
		runOn(t, e, "SET", "k", "v", "EX", "60")
		require.LessOrEqual(t, e.space.TotalMemUsed(), budget)
		if e.space.TotalKeys() == 0 {
			require.Zero(t, e.KeysWithExpiry())
			require.Zero(t, e.space.TotalMemUsed())
		}
	}
}

func TestAllTypesExpireAndSurviveRewrite(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	creators := [][]string{
		{"SET", "k", "v"}, {"HSET", "k", "f", "v"}, {"RPUSH", "k", "v"}, {"SADD", "k", "v"},
		{"ZADD", "k", "1", "v"}, {"BF.ADD", "k", "v"}, {"CMS.INITBYDIM", "k", "10", "3"},
		{"MORRIS.INITBYDIM", "k", "10", "3"}, {"PFADD", "k", "v"}, {"CF.ADD", "k", "v"},
	}
	for _, creator := range creators {
		t.Run(creator[0], func(t *testing.T) {
			path := withAOFOn(t, e, func() {
				runOn(t, e, creator[0], creator[1:]...)
				require.NotNil(t, runOn(t, e, "MEMORY", "USAGE", "k"))
				require.EqualValues(t, 1, runOn(t, e, "EXPIRE", "k", "3600", "NX"))
				require.EqualValues(t, 0, runOn(t, e, "EXPIRE", "k", "7200", "NX"))
				require.EqualValues(t, 1, runOn(t, e, "EXPIRE", "k", "7200", "GT"))
				require.NoError(t, e.RewriteAOF())
			})
			restartOn(t, e, path)
			require.Greater(t, runOn(t, e, "TTL", "k").(int64), int64(7000))
			require.EqualValues(t, 1, runOn(t, e, "PERSIST", "k"))
			require.EqualValues(t, -1, runOn(t, e, "PTTL", "k"))
			owner, _ := e.space.OwnerOf("k")
			owner.SetExpiryAt("k", 1)
			for i := 0; i < 20 && e.space.TotalKeys() > 0; i++ {
				e.ExpireCycle()
			}
			require.Zero(t, e.space.TotalKeys())
			require.Zero(t, e.space.TotalMemUsed())
		})
	}
}

func TestSETOptionsReplayEffects(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	until := strconv.FormatInt(time.Now().Add(time.Hour).UnixMilli(), 10)
	path := withAOFOn(t, e, func() {
		require.Equal(t, "OK", runOn(t, e, "SET", "k", "one", "NX", "PXAT", until))
		require.Equal(t, "one", runOn(t, e, "SET", "k", "two", "XX", "GET", "KEEPTTL"))
		require.Equal(t, "two", runOn(t, e, "SET", "k", "ignored", "NX", "GET"))
		require.Equal(t, "two", runOn(t, e, "GET", "k"))
		require.Greater(t, runOn(t, e, "TTL", "k").(int64), int64(3500))
	})
	restartOn(t, e, path)
	require.Equal(t, "two", runOn(t, e, "GET", "k"))
	require.Greater(t, runOn(t, e, "TTL", "k").(int64), int64(3500))
}

func TestTornTailRepairSurvivesSecondRestart(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := filepath.Join(t.TempDir(), "log.aof")
	body := appendCommand(nil, "SET", "before", "yes")
	body = append(body, []byte("*3\r\n$3\r\nSET\r\n$3\r\nkey\r\n$9\r\nx")...)
	require.NoError(t, os.WriteFile(path, body, 0600))
	_, err := e.LoadAOF(path)
	require.True(t, IsTruncatedAOF(err))
	require.NoError(t, RepairAOFTail(err))
	require.NoError(t, e.OpenAOF(path))
	runOn(t, e, "SET", "after", "yes")
	require.NoError(t, e.CloseAOF())
	e.resetStores()
	_, err = e.LoadAOF(path)
	require.NoError(t, err)
	require.Equal(t, "yes", runOn(t, e, "GET", "before"))
	require.Equal(t, "yes", runOn(t, e, "GET", "after"))
}

func TestIdleFsyncAndStickyFailure(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	oldSync := e.aofSync
	defer func() { e.CloseAOF(); e.aofSync = oldSync }()
	reconfigure(t, e, func(o *Options) { o.Fsync = FsyncEverySec })
	require.NoError(t, e.OpenAOF(filepath.Join(t.TempDir(), "log")))
	calls := 0
	e.aofSync = func(*os.File) error { calls++; return nil }
	runOn(t, e, "SET", "k", "v")
	require.NoError(t, e.FlushAOF())
	waitForRewriteSyncOn(t, e)
	require.Zero(t, calls)
	e.aof.lastSync = time.Now().Add(-2 * time.Second)
	require.NoError(t, e.FlushAOF())
	waitForRewriteSyncOn(t, e)
	e.pollAOFSync(true)
	require.Equal(t, 1, calls)
	require.False(t, e.aof.dirty)
	reconfigure(t, e, func(o *Options) { o.Fsync = FsyncAlways })
	diskErr := errors.New("injected sync failure")
	e.aofSync = func(*os.File) error { return diskErr }
	runOn(t, e, "SET", "k", "next")
	require.ErrorIs(t, e.FlushAOF(), diskErr)
	e.aofSync = oldSync
	require.ErrorIs(t, e.FlushAOF(), diskErr)
}

func TestDumpCollectionsAndChecksum(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	for _, cmd := range [][]string{{"HSET", "h", "f", "v"}, {"RPUSH", "l", "a", "b"}} {
		runOn(t, e, cmd[0], cmd[1:]...)
		payload, _ := e.dumpKey(cmd[1])
		require.NoError(t, e.restoreKey("copy", payload))
		require.Equal(t, runOn(t, e, "TYPE", cmd[1]), runOn(t, e, "TYPE", "copy"))
		payload[len(payload)-1] ^= 1
		require.Error(t, e.restoreKey("copy", payload))
	}
}

func FuzzRestoreValidation(f *testing.F) {
	f.Add([]byte{8})
	f.Add([]byte("KEL1"))
	f.Add([]byte{1, 'v'})
	f.Fuzz(func(t *testing.T, p []byte) {
		if len(p) > 1<<20 {
			return
		}
		ResetStores()
		_ = defaultEngine.restoreKey("fuzz", p)
	})
}

func TestCollectionRangesAndTrimPersistence(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := withAOFOn(t, e, func() {
		runOn(t, e, "ZADD", "z", "1", "a", "1", "b", "2", "c")
		require.Equal(t, []interface{}{"c", "2", "b", "1"}, runOn(t, e, "ZRANGE", "z", "0", "1", "REV", "WITHSCORES"))
		require.Equal(t, []interface{}{"b", "c"}, runOn(t, e, "ZRANGE", "z", "-2", "-1"))
		runOn(t, e, "RPUSH", "l", "a", "b", "c", "d")
		runOn(t, e, "PEXPIRE", "l", "60000")
		runOn(t, e, "LTRIM", "l", "1", "-2")
		require.Equal(t, []interface{}{"b", "c"}, runOn(t, e, "LRANGE", "l", "0", "-1"))
	})
	restartOn(t, e, path)
	require.Equal(t, []interface{}{"b", "c"}, runOn(t, e, "LRANGE", "l", "0", "-1"))
	require.Greater(t, runOn(t, e, "PTTL", "l").(int64), int64(50000))
}

func TestSmallBloomDumpRoundTrip(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "BF.RESERVE", "b", "0.9", "1")
	payload, ok := e.dumpKey("b")
	require.True(t, ok)
	require.NoError(t, e.restoreKey("copy", payload))
	runOn(t, e, "BF.ADD", "copy", "member")
	require.Equal(t, int64(1), runOn(t, e, "BF.EXISTS", "copy", "member"))
}

func TestExpirySamplingDoesNotStarveCollections(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	for i := 0; i < 100; i++ {
		key := strconv.Itoa(i)
		runOn(t, e, "SET", key, "v", "EX", "600")
	}
	runOn(t, e, "HSET", "h", "f", "v")
	e.hashStore.SetExpiryAt("h", 1)
	for i := 0; i < 20; i++ {
		e.ExpireCycle()
	}
	require.Zero(t, e.hashStore.Len())
}

func TestReplayExpiryDoesNotResurrectHistoricalMutations(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := withAOFOn(t, e, func() {
		runOn(t, e, "SET", "n", "9", "PX", "100")
		runOn(t, e, "INCR", "n")
		runOn(t, e, "HSET", "h", "a", "1")
		runOn(t, e, "PEXPIRE", "h", "100")
		runOn(t, e, "HSET", "h", "b", "2")
		runOn(t, e, "CMS.INITBYDIM", "cms", "10", "2")
		runOn(t, e, "PEXPIRE", "cms", "100")
		runOn(t, e, "CMS.INCRBY", "cms", "item", "1")
	})
	time.Sleep(120 * time.Millisecond)
	restartOn(t, e, path)
	require.Zero(t, e.space.TotalKeys())
	require.Zero(t, e.space.TotalMemUsed())
}

func TestStartupExpiryIsLoggedBeforeKeyReuse(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := withAOFOn(t, e, func() {
		runOn(t, e, "SET", "n", "9", "PX", "100")
		runOn(t, e, "HSET", "h", "old", "value")
		runOn(t, e, "PEXPIRE", "h", "100")
	})
	time.Sleep(120 * time.Millisecond)
	restartOn(t, e, path)
	require.Zero(t, e.space.TotalKeys())
	require.NoError(t, e.OpenAOF(path))
	runOn(t, e, "INCR", "n")
	runOn(t, e, "HSET", "h", "new", "value")
	require.NoError(t, e.CloseAOF())
	restartOn(t, e, path)
	require.Equal(t, "1", runOn(t, e, "GET", "n"))
	require.Equal(t, int64(1), runOn(t, e, "HLEN", "h"))
	require.Equal(t, "value", runOn(t, e, "HGET", "h", "new"))
	require.Equal(t, int64(-1), runOn(t, e, "PTTL", "h"))
}

func TestStartupEvictionsRemainDeletedAfterBudgetIncrease(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	defer e.resetStores()
	reconfigure(t, e, func(o *Options) { o.MaxKeys = 100 })
	path := withAOFOn(t, e, func() { runOn(t, e, "SET", "a", "1"); runOn(t, e, "SET", "b", "2") })
	reconfigure(t, e, func(o *Options) { o.MaxKeys = 1 })
	restartOn(t, e, path)
	require.Equal(t, 1, e.space.TotalKeys())
	require.NoError(t, e.OpenAOF(path))
	require.NoError(t, e.CloseAOF())
	reconfigure(t, e, func(o *Options) { o.MaxKeys = 100 })
	restartOn(t, e, path)
	require.Equal(t, 1, e.space.TotalKeys())
}

func TestLazyExpiryIsLoggedBeforeRecreation(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := withAOFOn(t, e, func() {
		runOn(t, e, "SET", "n", "9", "PX", "100")
		runOn(t, e, "HSET", "h", "old", "value")
		runOn(t, e, "PEXPIRE", "h", "100")
		time.Sleep(120 * time.Millisecond)
		require.Equal(t, int64(1), runOn(t, e, "INCR", "n"))
		require.Equal(t, int64(1), runOn(t, e, "HSET", "h", "new", "value"))
	})
	restartOn(t, e, path)
	require.Equal(t, "1", runOn(t, e, "GET", "n"))
	require.Equal(t, int64(1), runOn(t, e, "HLEN", "h"))
	require.Equal(t, "value", runOn(t, e, "HGET", "h", "new"))
}

func TestBackgroundSyncDoesNotBlockAndPreservesLaterWrites(t *testing.T) {
	// Not parallel: it asserts on wall-clock time, which tests running
	// beside it would stretch.
	e := newTestEngine(t, Options{})
	oldSync := e.aofSync
	release := make(chan struct{})
	defer func() { e.CloseAOF(); e.aofSync = oldSync }()
	reconfigure(t, e, func(o *Options) { o.Fsync = FsyncEverySec })
	require.NoError(t, e.OpenAOF(filepath.Join(t.TempDir(), "log")))
	diskErr := errors.New("background sync failed")
	e.aofSync = func(*os.File) error { <-release; return diskErr }
	runOn(t, e, "SET", "first", "value")
	e.aof.lastSync = time.Now().Add(-2 * time.Second)
	// A blocked Sync must not block this call. The timer also releases the fake
	// disk if a regression blocks, so the test fails rather than hanging CI.
	timer := time.AfterFunc(10*time.Second, func() { close(release) })
	started := time.Now()
	require.NoError(t, e.FlushAOF())
	waitForRewriteSyncOn(t, e)
	elapsed := time.Since(started)
	runOn(t, e, "SET", "later", "value")
	require.NoError(t, e.FlushAOF())
	waitForRewriteSyncOn(t, e)
	require.True(t, e.aof.dirty)
	require.NotNil(t, e.aof.syncPending)
	if timer.Stop() {
		close(release)
	}
	e.pollAOFSync(true)
	require.Less(t, elapsed, 500*time.Millisecond)
	require.ErrorIs(t, e.FlushAOF(), diskErr)
	require.ErrorIs(t, e.CloseAOF(), diskErr)
}

func TestLargeListRewriteRestartsAfterMutation(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	for _, mutation := range []string{"append", "replace", "delete"} {
		t.Run(mutation, func(t *testing.T) {
			e.resetStores()
			path := filepath.Join(t.TempDir(), "log")
			require.NoError(t, e.OpenAOF(path))
			defer e.CloseAOF()
			values := []string{"large"}
			for i := 0; i < 2000; i++ {
				values = append(values, strconv.Itoa(i))
			}
			runOn(t, e, "RPUSH", values...)
			runOn(t, e, "PEXPIRE", "large", "60000")
			require.NoError(t, e.StartRewrite())
			require.NoError(t, e.AdvanceRewrite())
			waitForRewriteSyncOn(t, e)
			require.True(t, e.rewrite.collectionActive, "large list should yield between chunks")
			switch mutation {
			case "append":
				runOn(t, e, "RPUSH", "large", "last")
			case "replace":
				runOn(t, e, "DEL", "large")
				runOn(t, e, "SET", "large", "replacement")
			case "delete":
				runOn(t, e, "DEL", "large")
			}
			require.False(t, e.rewrite.collectionActive)
			for i := 0; e.rewrite.active && i < 100; i++ {
				require.NoError(t, e.FlushAOF())
				waitForRewriteSyncOn(t, e)
			}
			require.False(t, e.rewrite.active)
			require.NoError(t, e.CloseAOF())
			e.resetStores()
			_, err := e.LoadAOF(path)
			require.NoError(t, err)
			switch mutation {
			case "append":
				l, ok := e.listStore.Peek("large")
				require.True(t, ok)
				require.Equal(t, 2001, l.Len())
				first, _ := l.Index(0)
				last, _ := l.Index(-1)
				require.Equal(t, "0", first)
				require.Equal(t, "last", last)
				_, has := e.listStore.GetExpiry("large")
				require.True(t, has)
			case "replace":
				require.Equal(t, "replacement", e.dictStore.Peek("large").Value)
			case "delete":
				require.Nil(t, e.dictStore.Peek("large"))
				_, ok := e.listStore.Peek("large")
				require.False(t, ok)
			}
		})
	}
}

func TestRewriteAndCloseFenceBackgroundSync(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	oldSync := e.aofSync
	defer func() { e.CloseAOF(); e.aofSync = oldSync }()
	reconfigure(t, e, func(o *Options) { o.Fsync = FsyncEverySec })
	require.NoError(t, e.OpenAOF(filepath.Join(t.TempDir(), "log")))
	release := make(chan struct{})
	timer := time.AfterFunc(time.Second, func() { close(release) })
	defer func() {
		if timer.Stop() {
			close(release)
		}
	}()
	e.aofSync = func(f *os.File) error { <-release; _, err := f.Stat(); return err }
	runOn(t, e, "SET", "k", "v")
	e.aof.lastSync = time.Now().Add(-2 * time.Second)
	require.NoError(t, e.FlushAOF())
	waitForRewriteSyncOn(t, e)
	oldFile := e.aof.file
	require.NoError(t, e.StartRewrite())
	require.NoError(t, e.AdvanceRewrite())
	waitForRewriteSyncOn(t, e)
	require.True(t, e.rewrite.active)
	require.Same(t, oldFile, e.aof.file)
	// Close must join the worker before closing its file descriptor.
	e.CancelRewrite()
	require.NoError(t, e.CloseAOF())
	require.Nil(t, e.aof.syncPending)
}

func TestAsyncAppendBarrierAndFailure(t *testing.T) {
	// Not parallel: it asserts on wall-clock time, which tests running
	// beside it would stretch.
	e := newTestEngine(t, Options{})
	for _, fail := range []bool{false, true} {
		t.Run(strconv.FormatBool(fail), func(t *testing.T) {
			e.resetStores()
			oldWrite := e.aofWrite
			defer func() { e.CloseAOF(); e.aofWrite = oldWrite }()
			reconfigure(t, e, func(o *Options) { o.Fsync = FsyncAlways })
			path := filepath.Join(t.TempDir(), "log")
			require.NoError(t, e.OpenAOF(path))
			release := make(chan struct{})
			timer := time.AfterFunc(10*time.Second, func() { close(release) })
			defer func() {
				if timer.Stop() {
					close(release)
				}
			}()
			diskErr := errors.New("injected append failure")
			e.aofWrite = func(f *os.File, b []byte) (int, error) {
				<-release
				if fail {
					return 0, diskErr
				}
				return f.Write(b)
			}
			runOn(t, e, "SET", "k", "value")
			start := time.Now()
			ready, err := e.FlushAOFAsync(nil)
			require.NoError(t, err)
			require.False(t, ready)
			require.Less(t, time.Since(start), 500*time.Millisecond)
			require.True(t, e.AppendPending())
			ready, err = e.FlushAOFAsync(nil)
			require.NoError(t, err)
			require.False(t, ready)
			if timer.Stop() {
				close(release)
			}
			e.pollAppend(true)
			ready, err = e.FlushAOFAsync(nil)
			if fail {
				require.ErrorIs(t, err, diskErr)
				require.False(t, ready)
			} else {
				require.NoError(t, err)
				require.True(t, ready)
				require.NoError(t, e.CloseAOF())
				e.resetStores()
				_, err = e.LoadAOF(path)
				require.NoError(t, err)
				require.Equal(t, "value", e.dictStore.Peek("k").Value)
			}
		})
	}
}
