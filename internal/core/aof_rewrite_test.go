package core

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/brandopakel/keel/internal/constant"
	"github.com/brandopakel/keel/internal/data_structure"
	"github.com/brandopakel/keel/internal/testlock"
)

// fillOneOfEverythingOn writes a key of every type on e, so a rewrite has to
// carry all eight keyspaces and not merely the ones a command can rebuild.
func fillOneOfEverythingOn(t *testing.T, e *Engine) {
	t.Helper()
	runOn(t, e, "SET", "str", "hello")
	runOn(t, e, "SET", "num", "41")
	runOn(t, e, "INCR", "num")
	runOn(t, e, "SET", "living", "v", "EX", "1000")
	runOn(t, e, "SADD", "set", "a", "b", "c")
	runOn(t, e, "ZADD", "z", "1.5", "alice", "-2.25", "bob")
	runOn(t, e, "GEOADD", "geo", "13.361389", "38.115556", "palermo")
	runOn(t, e, "PFADD", "hll", "x", "y", "z")
	runOn(t, e, "BF.MADD", "bf", "member")
	runOn(t, e, "CF.ADD", "cf", "member")
	runOn(t, e, "CMS.INITBYDIM", "cms", "100", "5")
	runOn(t, e, "CMS.INCRBY", "cms", "item", "7")
	runOn(t, e, "MORRIS.INITBYDIM", "mor", "200", "5")
	runOn(t, e, "MORRIS.INCRBY", "mor", "hits", "500000")
}

// snapshotEverythingOn reads back from e every value a test compares across a
// rewrite.
func snapshotEverythingOn(t *testing.T, e *Engine) map[string]interface{} {
	t.Helper()
	set := runOn(t, e, "SMEMBERS", "set").([]interface{})
	members := make([]string, 0, len(set))
	for _, m := range set {
		members = append(members, m.(string))
	}
	sortStrings(members)
	return map[string]interface{}{
		"str":     runOn(t, e, "GET", "str"),
		"num":     runOn(t, e, "GET", "num"),
		"living":  runOn(t, e, "GET", "living"),
		"set":     strings.Join(members, ","),
		"z.alice": runOn(t, e, "ZSCORE", "z", "alice"),
		"z.bob":   runOn(t, e, "ZSCORE", "z", "bob"),
		"zcard":   runOn(t, e, "ZCARD", "z"),
		"geo":     runOn(t, e, "GEOHASH", "geo", "palermo"),
		"hll":     runOn(t, e, "PFCOUNT", "hll"),
		"bf":      runOn(t, e, "BF.EXISTS", "bf", "member"),
		"cf":      runOn(t, e, "CF.EXISTS", "cf", "member"),
		"cms":     runOn(t, e, "CMS.QUERY", "cms", "item"),
		"mor":     runOn(t, e, "MORRIS.QUERY", "mor", "hits"),
		"keys":    e.space.TotalKeys(),
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// TestRewriteReproducesEveryKeyspace is the contract: replaying the rewritten
// log must give back exactly what was there, for all eight types.
func TestRewriteReproducesEveryKeyspace(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := filepath.Join(t.TempDir(), "rw.aof")
	assert.NoError(t, e.OpenAOF(path))
	fillOneOfEverythingOn(t, e)
	assert.NoError(t, e.FlushAOF())
	waitForRewriteSyncOn(t, e)

	before := snapshotEverythingOn(t, e)
	assert.NoError(t, e.RewriteAOF())
	assert.NoError(t, e.CloseAOF())

	e.resetStores()
	_, err := e.LoadAOF(path)
	assert.NoError(t, err)
	assert.Equal(t, before, snapshotEverythingOn(t, e),
		"a rewritten log must reproduce the keyspace exactly")
}

// TestRewriteShrinksALogOfRepeatedWrites is the point of the whole exercise.
func TestRewriteShrinksALogOfRepeatedWrites(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := filepath.Join(t.TempDir(), "shrink.aof")
	assert.NoError(t, e.OpenAOF(path))

	// One key, written many times. Every write but the last is history.
	for i := 0; i < 5000; i++ {
		runOn(t, e, "SET", "hot", strconv.Itoa(i))
	}
	assert.NoError(t, e.FlushAOF())
	waitForRewriteSyncOn(t, e)
	grown, err := os.Stat(path)
	assert.NoError(t, err)

	assert.NoError(t, e.RewriteAOF())
	shrunk, err := os.Stat(path)
	assert.NoError(t, err)

	t.Logf("5000 writes to one key: %d bytes -> %d after rewrite", grown.Size(), shrunk.Size())
	assert.Less(t, shrunk.Size()*100, grown.Size(),
		"a log of one key written 5000 times must shrink by more than a hundredfold")

	assert.NoError(t, e.CloseAOF())
	e.resetStores()
	_, err = e.LoadAOF(path)
	assert.NoError(t, err)
	assert.EqualValues(t, "4999", runOn(t, e, "GET", "hot"), "and must still hold the last value written")
}

// TestRewriteDropsDeletedAndExpiredKeys. History includes keys that no longer
// exist, and carrying them would make the rewrite pointless for the workload
// where the log grows fastest.
func TestRewriteDropsDeletedAndExpiredKeys(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := filepath.Join(t.TempDir(), "drop.aof")
	assert.NoError(t, e.OpenAOF(path))

	for i := 0; i < 100; i++ {
		runOn(t, e, "SET", "gone"+strconv.Itoa(i), "v")
	}
	for i := 0; i < 100; i++ {
		runOn(t, e, "DEL", "gone"+strconv.Itoa(i))
	}
	runOn(t, e, "SET", "stays", "v")
	runOn(t, e, "SET", "expires", "v", "PX", "1")

	deadline := time.Now().Add(60 * time.Millisecond)
	for time.Now().Before(deadline) {
	}

	assert.NoError(t, e.RewriteAOF())
	assert.NoError(t, e.CloseAOF())

	data, _ := os.ReadFile(path)
	assert.NotContains(t, string(data), "gone", "deleted keys must not be carried forward")
	assert.NotContains(t, string(data), "expires", "nor keys whose expiry has passed")
	assert.Contains(t, string(data), "stays")

	e.resetStores()
	_, err := e.LoadAOF(path)
	assert.NoError(t, err)
	assert.Equal(t, 1, e.space.TotalKeys())
}

// TestRewriteKeepsExpiryAsAnInstant. A TTL survives a rewrite as the moment it
// falls due, not as the time remaining when the rewrite happened to run.
func TestRewriteKeepsExpiryAsAnInstant(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := filepath.Join(t.TempDir(), "ttl.aof")
	assert.NoError(t, e.OpenAOF(path))
	runOn(t, e, "SET", "k", "v", "EX", "1000")

	assert.NoError(t, e.RewriteAOF())
	assert.NoError(t, e.CloseAOF())

	data, _ := os.ReadFile(path)
	assert.Contains(t, string(data), "PEXPIREAT")

	e.resetStores()
	_, err := e.LoadAOF(path)
	assert.NoError(t, err)
	assert.InDelta(t, 1000, runOn(t, e, "TTL", "k"), 2)
}

// TestWritesAfterARewriteAreAppendedToTheNewLog.
//
// The old file is renamed away, so a descriptor still pointing at it writes to
// a file with no name - the writes vanish at the next restart, which is the
// quietest possible way to lose data.
func TestWritesAfterARewriteAreAppendedToTheNewLog(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := filepath.Join(t.TempDir(), "after.aof")
	assert.NoError(t, e.OpenAOF(path))
	runOn(t, e, "SET", "before", "1")
	assert.NoError(t, e.RewriteAOF())

	runOn(t, e, "SET", "after", "2")
	assert.NoError(t, e.FlushAOF())
	waitForRewriteSyncOn(t, e)
	assert.NoError(t, e.CloseAOF())

	e.resetStores()
	_, err := e.LoadAOF(path)
	assert.NoError(t, err)
	assert.EqualValues(t, "1", runOn(t, e, "GET", "before"))
	assert.EqualValues(t, "2", runOn(t, e, "GET", "after"), "a write after the rewrite must survive")
}

// TestRewriteLeavesNoTemporaryFile. The new log is built beside the old one and
// renamed over it; a leftover .rewrite file means the swap did not complete.
func TestRewriteLeavesNoTemporaryFile(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	dir := t.TempDir()
	path := filepath.Join(dir, "tmp.aof")
	assert.NoError(t, e.OpenAOF(path))
	runOn(t, e, "SET", "k", "v")
	assert.NoError(t, e.RewriteAOF())
	assert.NoError(t, e.CloseAOF())

	entries, err := os.ReadDir(dir)
	assert.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), ".rewrite", "the temporary file must be gone")
	}
}

// TestAutomaticRewriteTriggersOnGrowth.
func TestAutomaticRewriteTriggersOnGrowth(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	for _, delayedSync := range []bool{false, true} {
		t.Run("delayed-sync="+strconv.FormatBool(delayedSync), func(t *testing.T) {
			testAutomaticRewriteTriggersOnGrowth(t, e, delayedSync)
		})
	}
}

func testAutomaticRewriteTriggersOnGrowth(t *testing.T, e *Engine, delayedSync bool) {
	t.Helper()
	syncFile := e.aofSync
	defer func() {
		e.CloseAOF()
		e.aofSync = syncFile
	}()
	withOptionsOn(t, e, func(o *Options) {
		o.AutoRewritePercentage, o.AutoRewriteMinSize, o.Fsync = 100, 4096, FsyncEverySec
	})

	path := filepath.Join(t.TempDir(), "auto.aof")
	e.resetStores()
	require.NoError(t, e.OpenAOF(path))
	releaseSync := func() {}
	if delayedSync {
		release := make(chan struct{})
		releaseSync = func() {
			if release != nil {
				close(release)
				release = nil
			}
		}
		defer releaseSync()
		blocked := release
		e.aofSync = func(f *os.File) error { <-blocked; return f.Sync() }
		e.aof.lastSync = time.Now().Add(-2 * time.Second)
	}
	for i := 0; i < 4000; i++ {
		runOn(t, e, "SET", "hot", strconv.Itoa(i))
		require.NoError(t, e.FlushAOF())
		waitForRewriteSyncOn(t, e)
	}
	if delayedSync {
		require.True(t, e.RewriteActive(), "file replacement must wait for the sync worker")
		require.Less(t, e.rewrite.written, int64(4096), "waiting for sync must not repeatedly serialize the hot key")
		size, err := os.Stat(path)
		require.NoError(t, err)
		require.Greater(t, size.Size(), int64(64*1024), "the old log grows while sync holds its descriptor")
	}
	releaseSync()
	// A background everysec sync keeps the old descriptor alive and can delay
	// replacement past the final write. Check size only after the automatic
	// rewrite has completed, including its idle event-loop turns.
	e.pollAOFSync(true)
	require.NoError(t, e.FlushAOF())
	waitForRewriteSyncOn(t, e)
	for i := 0; e.RewriteActive() && i < 100; i++ {
		e.pollAOFSync(true)
		require.NoError(t, e.FlushAOF())
		waitForRewriteSyncOn(t, e)
	}
	require.False(t, e.RewriteActive(), "automatic rewrite must finish after sync completes")

	_, _, rewrites, _ := e.AOFStats()
	assert.Greater(t, rewrites, 0, "a log growing past the threshold must rewrite itself")

	size, err := os.Stat(path)
	assert.NoError(t, err)
	assert.Less(t, size.Size(), int64(64*1024),
		"and must stay small, rather than growing between rewrites forever")

	assert.NoError(t, e.CloseAOF())
	e.resetStores()
	_, err = e.LoadAOF(path)
	assert.NoError(t, err)
	assert.EqualValues(t, "3999", runOn(t, e, "GET", "hot"))
}

// TestAutomaticRewriteCanBeTurnedOff.
func TestAutomaticRewriteCanBeTurnedOff(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	reconfigure(t, e, func(o *Options) { o.AutoRewritePercentage, o.AutoRewriteMinSize = Off, 1 })

	path := filepath.Join(t.TempDir(), "off.aof")
	e.resetStores()
	assert.NoError(t, e.OpenAOF(path))
	for i := 0; i < 2000; i++ {
		runOn(t, e, "SET", "hot", strconv.Itoa(i))
		assert.NoError(t, e.FlushAOF())
		waitForRewriteSyncOn(t, e)
	}
	_, _, rewrites, _ := e.AOFStats()
	assert.Equal(t, 0, rewrites, "zero percentage must disable automatic rewriting")
	assert.NoError(t, e.CloseAOF())
}

func TestRewriteWithoutAppendonlyIsAnError(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.Error(t, e.RewriteAOF(), "there is nothing to rewrite when the log is off")
}

// TestRewriteStallProfile measures event-loop work separately from background
// sync waits. Snapshot traversal is incremental; filesystem calls and atomic
// value serialization can still exceed cooperative targets.
func TestRewriteStallProfile(t *testing.T) {
	// Not parallel: it profiles a rewrite's wall-clock stalls, which tests
	// running beside it would inflate.
	if testing.Short() {
		t.Skip("builds a million keys")
	}
	if raceEnabled {
		// This is a latency diagnostic. The separate slice-budget/replay test
		// covers correctness under race instrumentation.
		t.Skip("measures wall-clock latency, which -race inflates about fourfold")
	}
	// About 106 MiB of logs: held apart from cmd/keel's largest writer, and
	// taken before the engine's directory, so that it is released only once
	// those files are gone.
	testlock.HoldDiskHeavy(t)
	e := newTestEngine(t, Options{})
	path := filepath.Join(t.TempDir(), "profile.aof")
	require.NoError(t, e.OpenAOF(path))
	// A diagnostic deadline must not leave a worker or old keyspace traversal
	// behind for the next test, even when require stops this test early.
	t.Cleanup(func() { assert.NoError(t, e.CloseAOF()); e.resetStores() })
	const keys = 1000000
	for i := 0; i < keys; i++ {
		runOn(t, e, "SET", "key:"+strconv.Itoa(i), "value-of-some-length")
	}
	assert.NoError(t, e.FlushAOF())
	waitForRewriteSyncOn(t, e)

	start := time.Now()
	assert.NoError(t, e.StartRewrite())
	collecting := time.Since(start)

	var walk []time.Duration
	var final, waiting, longestWait time.Duration
	longestPhase := "none"
	total := collecting
	for {
		waitStart := time.Now()
		phase := "ready"
		if e.pendingRewriteIO != nil {
			phase = "replacement write"
			if e.pendingRewriteIO.body == nil {
				phase = "replacement sync"
			}
		} else if e.aof.syncPending != nil {
			phase = "original AOF sync"
		}
		slow := false
		for e.RewriteActive() && !e.RewriteNeedsCycle() {
			waited := time.Since(waitStart)
			if waited >= 3*time.Second && !slow {
				t.Logf("rewrite worker exceeded three-second diagnostic threshold: phase=%s written=%d", phase, e.rewrite.written)
				slow = true
			}
			// This is a deadlock watchdog, not a storage-latency assertion.
			// Correctness below requires a committed rewrite and bounded work.
			require.Less(t, waited, time.Minute,
				"rewrite diagnostic watchdog expired: phase=%s written=%d", phase, e.rewrite.written)
			time.Sleep(time.Millisecond)
		}
		waited := time.Since(waitStart)
		waiting += waited
		if waited > longestWait {
			longestWait, longestPhase = waited, phase
		}
		t0 := time.Now()
		require.NoError(t, e.AdvanceRewrite())
		more := e.RewriteActive()
		took := time.Since(t0)
		total += took
		if more {
			walk = append(walk, took)
			continue
		}
		final = took
		break
	}

	slices.Sort(walk)
	median := walk[len(walk)/2]
	worst := walk[len(walk)-1]
	t.Logf("%d keys: collecting %v, %d walk slices median %v worst %v, final %v, loop work %v, worker waits %v",
		keys, collecting.Round(time.Millisecond), len(walk),
		median.Round(time.Microsecond), worst.Round(time.Microsecond),
		final.Round(time.Millisecond), total.Round(time.Millisecond), waiting.Round(time.Millisecond))
	t.Logf("longest worker wait %v (%s); three-second threshold is diagnostic only",
		longestWait.Round(time.Microsecond), longestPhase)

	// Shared-host scheduling and storage affect even the median. Correctness
	// uses work bounds; the scheduled-probe harness measures latency separately.
	assert.Greater(t, len(walk), 100, "the walk must be spread over many cycles")
	assert.Equal(t, 1, e.aof.rewrites, "the rewrite must actually commit")
	assert.NoError(t, e.CloseAOF())
}

// BenchmarkRewrite measures the stall a rewrite costs, which is the number the
// package comment has to be able to quote. It blocks the event loop, so what a
// client sees is this figure in full.
func BenchmarkRewrite(b *testing.B) {
	for _, keys := range []int{10000, 100000, 1000000} {
		b.Run(strconv.Itoa(keys)+"-string-keys", func(b *testing.B) {
			e := newTestEngine(b, Options{})
			path := filepath.Join(b.TempDir(), "bench.aof")
			if err := e.OpenAOF(path); err != nil {
				b.Fatal(err)
			}
			defer e.CloseAOF()
			var w replyWriter
			for i := 0; i < keys; i++ {
				e.EvalAndResponse(&Command{Cmd: "SET",
					Args: []string{"key:" + strconv.Itoa(i), "value-of-some-length"}}, &w)
				w.b = w.b[:0]
			}
			if err := e.FlushAOF(); err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := e.RewriteAOF(); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(keys)*float64(b.N)/b.Elapsed().Seconds(), "keys/s")
		})
	}
}

// stepRewriteOn advances e's rewrite by one slice, the way an event-loop cycle
// would, and reports whether it is still going.
func stepRewriteOn(t *testing.T, e *Engine) bool {
	t.Helper()
	assert.NoError(t, e.AdvanceRewrite())
	waitForRewriteSyncOn(t, e)
	return e.RewriteActive()
}

// TestRewriteSeesTheKeyspaceMoveAndStillGetsItRight is the property the
// incremental walk rests on.
//
// The walk takes many cycles, so the keyspace changes underneath it: keys are
// written after it passed them, created after it would have reached them, and
// deleted before it gets there. None of that has to be prevented, only noticed
// - every key written during the walk is written again at the end from what it
// holds then.
func TestRewriteSeesTheKeyspaceMoveAndStillGetsItRight(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := filepath.Join(t.TempDir(), "moving.aof")
	assert.NoError(t, e.OpenAOF(path))

	const keys = 20000
	for i := 0; i < keys; i++ {
		runOn(t, e, "SET", "k"+strconv.Itoa(i), "original")
	}
	assert.NoError(t, e.FlushAOF())
	waitForRewriteSyncOn(t, e)
	assert.NoError(t, e.StartRewrite())

	// One slice, so the walk is part way through and has certainly recorded
	// some keys and not others.
	assert.True(t, stepRewriteOn(t, e), "the walk should need more than one slice")

	// Now move the ground under it, in every way that matters.
	runOn(t, e, "SET", "k0", "changed-after-the-walk-passed")
	runOn(t, e, "SET", "k19999", "changed-before-the-walk-arrived")
	runOn(t, e, "DEL", "k1")
	runOn(t, e, "SET", "brand-new", "created-mid-rewrite")
	runOn(t, e, "SADD", "new-set", "a", "b")
	runOn(t, e, "SET", "k2", "1")
	runOn(t, e, "INCR", "k2")
	runOn(t, e, "INCR", "k2")

	for stepRewriteOn(t, e) {
	}
	assert.NoError(t, e.CloseAOF())

	before := map[string]interface{}{
		"k0":        runOn(t, e, "GET", "k0"),
		"k2":        runOn(t, e, "GET", "k2"),
		"k19999":    runOn(t, e, "GET", "k19999"),
		"brand-new": runOn(t, e, "GET", "brand-new"),
		"new-set":   runOn(t, e, "SCARD", "new-set"),
		"count":     e.space.TotalKeys(),
	}

	e.resetStores()
	_, err := e.LoadAOF(path)
	assert.NoError(t, err)

	assert.Equal(t, before["k0"], runOn(t, e, "GET", "k0"), "a key written after the walk passed it")
	assert.Equal(t, before["k19999"], runOn(t, e, "GET", "k19999"), "a key written before the walk reached it")
	assert.Equal(t, before["brand-new"], runOn(t, e, "GET", "brand-new"), "a key created mid-rewrite")
	assert.Equal(t, before["new-set"], runOn(t, e, "SCARD", "new-set"), "a set created mid-rewrite")
	assert.Equal(t, before["count"], e.space.TotalKeys(), "and no key resurrected or lost")

	// INCR is the one that catches a snapshot-plus-diff design applying an
	// operation twice: the value must be what it was, not what it would be if
	// the increments were replayed on top of a state that already had them.
	assert.EqualValues(t, "3", runOn(t, e, "GET", "k2"))
	assert.Equal(t, constant.RespNil, rawReplyOn(t, e, "GET", "k1"), "a key deleted mid-rewrite stays deleted")
}

// TestRewriteOverridesASetRatherThanMergingWithIt.
//
// A set recorded by the walk and then changed would, without the DEL the
// override writes first, come back as the union of both versions - so members
// removed during the rewrite would be alive again after a restart.
func TestRewriteOverridesASetRatherThanMergingWithIt(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := filepath.Join(t.TempDir(), "merge.aof")
	assert.NoError(t, e.OpenAOF(path))
	runOn(t, e, "SADD", "s", "a", "b", "c", "d")
	// Padding, so the walk records the set and then has slices left to run.
	for i := 0; i < 5000; i++ {
		runOn(t, e, "SET", "pad"+strconv.Itoa(i), "v")
	}
	assert.NoError(t, e.FlushAOF())
	waitForRewriteSyncOn(t, e)

	assert.NoError(t, e.StartRewrite())
	assert.True(t, stepRewriteOn(t, e))
	runOn(t, e, "SREM", "s", "a", "b")
	for stepRewriteOn(t, e) {
	}
	assert.NoError(t, e.CloseAOF())

	e.resetStores()
	_, err := e.LoadAOF(path)
	assert.NoError(t, err)
	assert.EqualValues(t, 2, runOn(t, e, "SCARD", "s"),
		"members removed during a rewrite must not come back")
}

// TestRewriteDoesNotCountAsUse.
//
// The walk reads every key in the keyspace. If that counted as an access,
// eviction would come out of a rewrite believing everything was equally hot and
// with no idea what anyone had actually asked for.
func TestRewriteDoesNotCountAsUse(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := filepath.Join(t.TempDir(), "lru.aof")
	assert.NoError(t, e.OpenAOF(path))
	for i := 0; i < 100; i++ {
		runOn(t, e, "SET", "k"+strconv.Itoa(i), "v")
	}
	// One key is genuinely hot, and must still look hot afterwards.
	for i := 0; i < 50; i++ {
		runOn(t, e, "GET", "k7")
	}
	hotBefore, ok := e.dictStore.ScoreOf("k7")
	assert.True(t, ok)
	coldBefore, ok := e.dictStore.ScoreOf("k42")
	assert.True(t, ok)
	assert.Greater(t, hotBefore, coldBefore)

	assert.NoError(t, e.RewriteAOF())

	hotAfter, _ := e.dictStore.ScoreOf("k7")
	coldAfter, _ := e.dictStore.ScoreOf("k42")
	assert.Equal(t, hotBefore, hotAfter, "a rewrite must not touch a key's access record")
	assert.Equal(t, coldBefore, coldAfter)
	assert.Greater(t, hotAfter, coldAfter, "and must leave the hot key still looking hot")
	assert.NoError(t, e.CloseAOF())
}

// Verify bounded work and complete output independently of scheduler, GC and
// filesystem timing. Log durations as diagnostics; benchmarks measure latency.
func TestRewriteSlicesObeyKeyBudgetAndReplay(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := filepath.Join(t.TempDir(), "slice.aof")
	assert.NoError(t, e.OpenAOF(path))
	for i := 0; i < 200000; i++ {
		runOn(t, e, "SET", "k"+strconv.Itoa(i), "some value of a realistic length")
	}
	assert.NoError(t, e.FlushAOF())
	waitForRewriteSyncOn(t, e)

	assert.NoError(t, e.StartRewrite())
	slices, worstWalk, final := 0, time.Duration(0), time.Duration(0)
	for {
		before := e.rewrite.pos
		start := time.Now()
		more := stepRewriteOn(t, e)
		took := time.Since(start)
		slices++
		assert.GreaterOrEqual(t, e.rewrite.pos-before, 0)
		assert.LessOrEqual(t, e.rewrite.pos-before, rewriteChunk,
			"every walk slice must obey its key budget, including the final slice")
		if more {
			if took > worstWalk {
				worstWalk = took
			}
		} else {
			// The last slice writes the keys that changed during the walk and
			// then syncs the file, so it is a different measurement from the
			// ones before it and is reported separately rather than averaged in.
			final = took
			break
		}
	}
	t.Logf("200,000 keys: %d slices, worst walk slice %v, final slice %v",
		slices, worstWalk, final)
	assert.Greater(t, slices, 50, "the walk must be spread over many cycles")
	assert.Equal(t, 1, e.aof.rewrites, "the rewrite must commit rather than fall back to the original log")
	assert.NoError(t, e.CloseAOF())
	e.resetStores()
	_, err := e.LoadAOF(path)
	assert.NoError(t, err)
	assert.Equal(t, 200000, e.dictStore.Len())
	for i := 0; i < 200000; i++ {
		assert.Equal(t, "some value of a realistic length", runOn(t, e, "GET", "k"+strconv.Itoa(i)))
	}
}

// TestCancelledRewriteLeavesTheOldLogIntact. A server stopping mid-rewrite must
// keep the log it has been appending to all along.
func TestCancelledRewriteLeavesTheOldLogIntact(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	dir := t.TempDir()
	path := filepath.Join(dir, "cancel.aof")
	assert.NoError(t, e.OpenAOF(path))
	for i := 0; i < 10000; i++ {
		runOn(t, e, "SET", "k"+strconv.Itoa(i), "v")
	}
	assert.NoError(t, e.FlushAOF())
	waitForRewriteSyncOn(t, e)

	assert.NoError(t, e.StartRewrite())
	assert.True(t, stepRewriteOn(t, e))
	e.CancelRewrite()
	assert.False(t, e.RewriteActive())

	runOn(t, e, "SET", "after-cancel", "v")
	assert.NoError(t, e.CloseAOF())

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), ".rewrite", "the abandoned file must be cleaned up")
	}

	e.resetStores()
	_, err := e.LoadAOF(path)
	assert.NoError(t, err)
	assert.Equal(t, 10001, e.space.TotalKeys(),
		"the old log must still hold everything, including writes after the cancellation")
}

// Reopening a log must not reset its growth threshold to accumulated history.
// Otherwise a restart every few minutes can defer automatic compaction forever.
func TestRestartDoesNotRatchetAutomaticRewriteBaseline(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	defer e.CloseAOF()
	reconfigure(t, e, func(o *Options) { o.AutoRewritePercentage, o.AutoRewriteMinSize = 100, 1024 })
	path := filepath.Join(t.TempDir(), "ratchet.aof")
	e.resetStores()
	assert.NoError(t, e.OpenAOF(path))
	// Use flushAOF to create a replayable historical log without auto-compaction.
	for i := 0; i < 200; i++ {
		runOn(t, e, "SET", "hot", strconv.Itoa(i))
		assert.NoError(t, e.flushAOF(false))
	}
	assert.NoError(t, e.CloseAOF())
	e.resetStores()
	_, err := e.LoadAOF(path)
	assert.NoError(t, err)
	assert.NoError(t, e.OpenAOF(path))
	assert.Greater(t, e.aof.baseSize, e.aof.rewriteBase)
	before := e.aof.baseSize
	for i := 0; i < 100 && e.aof.rewrites == 0; i++ {
		assert.NoError(t, e.FlushAOF())
		waitForRewriteSyncOn(t, e)
	}
	assert.Equal(t, 1, e.aof.rewrites)
	assert.Less(t, e.aof.baseSize, before/4)
	assert.Equal(t, e.aof.baseSize, e.aof.rewriteBase)
	assert.NoError(t, e.CloseAOF())
	e.resetStores()
	_, err = e.LoadAOF(path)
	assert.NoError(t, err)
	assert.Equal(t, "199", runOn(t, e, "GET", "hot"))
}

func TestRewriteStartDoesNotCopyKeyNames(t *testing.T) {
	// Not parallel: it reads the process's heap statistics, which a test
	// running beside it would move.
	e := newTestEngine(t, Options{})
	for _, n := range []int{1000, 100000} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			e.resetStores()
			require.NoError(t, e.OpenAOF(filepath.Join(t.TempDir(), "start.aof")))
			t.Cleanup(func() { e.CancelRewrite(); assert.NoError(t, e.CloseAOF()) })
			for i := 0; i < n; i++ {
				e.dictStore.Put("key:"+strconv.Itoa(i), e.dictStore.NewObj("v"))
			}
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			require.NoError(t, e.StartRewrite())
			runtime.ReadMemStats(&after)
			require.Empty(t, e.rewrite.keys, "start retains slot limits, not every name")
			require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(256<<10), "startup must not allocate an O(N) name slice")
			require.NoError(t, e.AdvanceRewrite())
			waitForRewriteSyncOn(t, e)
			require.LessOrEqual(t, len(e.rewrite.keys), data_structure.ScanMaxWork)
			require.LessOrEqual(t, e.rewrite.pos, data_structure.ScanMaxWork)
		})
	}
}

func TestRewriteRetainsBoundedNameBatches(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	if testing.Short() {
		t.Skip("builds a large keyspace")
	}
	assert.NoError(t, e.OpenAOF(filepath.Join(t.TempDir(), "held.aof")))
	defer func() { assert.NoError(t, e.CloseAOF()) }()

	const keys = 200000
	for i := 0; i < keys; i++ {
		runOn(t, e, "SET", "key:"+strconv.Itoa(i), "value-of-some-length")
	}
	assert.NoError(t, e.FlushAOF())
	waitForRewriteSyncOn(t, e)
	assert.NoError(t, e.StartRewrite())

	// Nothing is collected up front, so the walk starts holding nothing at all.
	assert.Zero(t, len(e.rewrite.keys), "starting a rewrite must not enumerate the keyspace")

	worst := 0
	for stepRewriteOn(t, e) {
		worst = max(worst, cap(e.rewrite.keys))
	}
	assert.Equal(t, 1, e.aof.rewrites, "the rewrite must commit")

	// Slice capacity may round above the work limit, but never scales with N.
	assert.LessOrEqual(t, worst, 2*data_structure.ScanMaxWork,
		"retained batch capacity must follow the fixed traversal budget")

	t.Logf("%d keys: walk retained at most %d names, %d walked", keys, worst, e.rewrite.pos)
}

// Refusing above the ceiling is a clean error rather than a started rewrite.
// The server has no key bound by default, so it warns at startup when it logs
// without a -maxkeys at or below the ceiling; cmd/keel tests that warning.
func TestRewriteCeilingRefusesCleanly(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.NoError(t, e.OpenAOF(filepath.Join(t.TempDir(), "ceiling.aof")))
	defer func() { assert.NoError(t, e.CloseAOF()) }()
	runOn(t, e, "SET", "k", "v")

	// Above the ceiling the refusal is immediate and leaves nothing running, so
	// the caller can tell a refusal from an abort part way through.
	old := e.keyCountForRewrite
	defer func() { e.keyCountForRewrite = old }()
	e.keyCountForRewrite = func() int { return RewriteKeyCeiling + 1 }
	err := e.StartRewrite()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "rewrite limit")
	assert.False(t, e.RewriteActive(), "a refused rewrite must not leave one started")

	// At the ceiling it proceeds.
	e.keyCountForRewrite = func() int { return RewriteKeyCeiling }
	assert.NoError(t, e.StartRewrite())
	assert.True(t, e.RewriteActive())
	for stepRewriteOn(t, e) {
	}
	assert.Equal(t, 1, e.aof.rewrites)
}
