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

	"github.com/brandopakel/keel/internal/config"
	"github.com/brandopakel/keel/internal/constant"
	"github.com/brandopakel/keel/internal/data_structure"
)

// fillOneOfEverything writes a key of every type, so a rewrite has to carry all
// eight keyspaces and not merely the ones a command can rebuild.
func fillOneOfEverything(t *testing.T) {
	t.Helper()
	run(t, "SET", "str", "hello")
	run(t, "SET", "num", "41")
	run(t, "INCR", "num")
	run(t, "SET", "living", "v", "EX", "1000")
	run(t, "SADD", "set", "a", "b", "c")
	run(t, "ZADD", "z", "1.5", "alice", "-2.25", "bob")
	run(t, "GEOADD", "geo", "13.361389", "38.115556", "palermo")
	run(t, "PFADD", "hll", "x", "y", "z")
	run(t, "BF.MADD", "bf", "member")
	run(t, "CF.ADD", "cf", "member")
	run(t, "CMS.INITBYDIM", "cms", "100", "5")
	run(t, "CMS.INCRBY", "cms", "item", "7")
	run(t, "MORRIS.INITBYDIM", "mor", "200", "5")
	run(t, "MORRIS.INCRBY", "mor", "hits", "500000")
}

// snapshotEverything reads back every value a test compares across a rewrite.
func snapshotEverything(t *testing.T) map[string]interface{} {
	t.Helper()
	set := run(t, "SMEMBERS", "set").([]interface{})
	members := make([]string, 0, len(set))
	for _, m := range set {
		members = append(members, m.(string))
	}
	sortStrings(members)
	return map[string]interface{}{
		"str":     run(t, "GET", "str"),
		"num":     run(t, "GET", "num"),
		"living":  run(t, "GET", "living"),
		"set":     strings.Join(members, ","),
		"z.alice": run(t, "ZSCORE", "z", "alice"),
		"z.bob":   run(t, "ZSCORE", "z", "bob"),
		"zcard":   run(t, "ZCARD", "z"),
		"geo":     run(t, "GEOHASH", "geo", "palermo"),
		"hll":     run(t, "PFCOUNT", "hll"),
		"bf":      run(t, "BF.EXISTS", "bf", "member"),
		"cf":      run(t, "CF.EXISTS", "cf", "member"),
		"cms":     run(t, "CMS.QUERY", "cms", "item"),
		"mor":     run(t, "MORRIS.QUERY", "mor", "hits"),
		"keys":    data_structure.TotalKeys(),
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
	path := filepath.Join(t.TempDir(), "rw.aof")
	ResetStores()
	assert.NoError(t, OpenAOF(path))
	fillOneOfEverything(t)
	assert.NoError(t, FlushAOF())
	waitForRewriteSync(t)

	before := snapshotEverything(t)
	assert.NoError(t, RewriteAOF())
	assert.NoError(t, CloseAOF())

	ResetStores()
	_, err := LoadAOF(path)
	assert.NoError(t, err)
	assert.Equal(t, before, snapshotEverything(t),
		"a rewritten log must reproduce the keyspace exactly")
}

// TestRewriteShrinksALogOfRepeatedWrites is the point of the whole exercise.
func TestRewriteShrinksALogOfRepeatedWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shrink.aof")
	ResetStores()
	assert.NoError(t, OpenAOF(path))

	// One key, written many times. Every write but the last is history.
	for i := 0; i < 5000; i++ {
		run(t, "SET", "hot", strconv.Itoa(i))
	}
	assert.NoError(t, FlushAOF())
	waitForRewriteSync(t)
	grown, err := os.Stat(path)
	assert.NoError(t, err)

	assert.NoError(t, RewriteAOF())
	shrunk, err := os.Stat(path)
	assert.NoError(t, err)

	t.Logf("5000 writes to one key: %d bytes -> %d after rewrite", grown.Size(), shrunk.Size())
	assert.Less(t, shrunk.Size()*100, grown.Size(),
		"a log of one key written 5000 times must shrink by more than a hundredfold")

	assert.NoError(t, CloseAOF())
	ResetStores()
	_, err = LoadAOF(path)
	assert.NoError(t, err)
	assert.EqualValues(t, "4999", run(t, "GET", "hot"), "and must still hold the last value written")
}

// TestRewriteDropsDeletedAndExpiredKeys. History includes keys that no longer
// exist, and carrying them would make the rewrite pointless for the workload
// where the log grows fastest.
func TestRewriteDropsDeletedAndExpiredKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "drop.aof")
	ResetStores()
	assert.NoError(t, OpenAOF(path))

	for i := 0; i < 100; i++ {
		run(t, "SET", "gone"+strconv.Itoa(i), "v")
	}
	for i := 0; i < 100; i++ {
		run(t, "DEL", "gone"+strconv.Itoa(i))
	}
	run(t, "SET", "stays", "v")
	run(t, "SET", "expires", "v", "PX", "1")

	deadline := time.Now().Add(60 * time.Millisecond)
	for time.Now().Before(deadline) {
	}

	assert.NoError(t, RewriteAOF())
	assert.NoError(t, CloseAOF())

	data, _ := os.ReadFile(path)
	assert.NotContains(t, string(data), "gone", "deleted keys must not be carried forward")
	assert.NotContains(t, string(data), "expires", "nor keys whose expiry has passed")
	assert.Contains(t, string(data), "stays")

	ResetStores()
	_, err := LoadAOF(path)
	assert.NoError(t, err)
	assert.Equal(t, 1, data_structure.TotalKeys())
}

// TestRewriteKeepsExpiryAsAnInstant. A TTL survives a rewrite as the moment it
// falls due, not as the time remaining when the rewrite happened to run.
func TestRewriteKeepsExpiryAsAnInstant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ttl.aof")
	ResetStores()
	assert.NoError(t, OpenAOF(path))
	run(t, "SET", "k", "v", "EX", "1000")

	assert.NoError(t, RewriteAOF())
	assert.NoError(t, CloseAOF())

	data, _ := os.ReadFile(path)
	assert.Contains(t, string(data), "PEXPIREAT")

	ResetStores()
	_, err := LoadAOF(path)
	assert.NoError(t, err)
	assert.InDelta(t, 1000, run(t, "TTL", "k"), 2)
}

// TestWritesAfterARewriteAreAppendedToTheNewLog.
//
// The old file is renamed away, so a descriptor still pointing at it writes to
// a file with no name - the writes vanish at the next restart, which is the
// quietest possible way to lose data.
func TestWritesAfterARewriteAreAppendedToTheNewLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "after.aof")
	ResetStores()
	assert.NoError(t, OpenAOF(path))
	run(t, "SET", "before", "1")
	assert.NoError(t, RewriteAOF())

	run(t, "SET", "after", "2")
	assert.NoError(t, FlushAOF())
	waitForRewriteSync(t)
	assert.NoError(t, CloseAOF())

	ResetStores()
	_, err := LoadAOF(path)
	assert.NoError(t, err)
	assert.EqualValues(t, "1", run(t, "GET", "before"))
	assert.EqualValues(t, "2", run(t, "GET", "after"), "a write after the rewrite must survive")
}

// TestRewriteLeavesNoTemporaryFile. The new log is built beside the old one and
// renamed over it; a leftover .rewrite file means the swap did not complete.
func TestRewriteLeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tmp.aof")
	ResetStores()
	assert.NoError(t, OpenAOF(path))
	run(t, "SET", "k", "v")
	assert.NoError(t, RewriteAOF())
	assert.NoError(t, CloseAOF())

	entries, err := os.ReadDir(dir)
	assert.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), ".rewrite", "the temporary file must be gone")
	}
}

// TestAutomaticRewriteTriggersOnGrowth.
func TestAutomaticRewriteTriggersOnGrowth(t *testing.T) {
	for _, delayedSync := range []bool{false, true} {
		t.Run("delayed-sync="+strconv.FormatBool(delayedSync), func(t *testing.T) {
			testAutomaticRewriteTriggersOnGrowth(t, delayedSync)
		})
	}
}

func testAutomaticRewriteTriggersOnGrowth(t *testing.T, delayedSync bool) {
	t.Helper()
	pct, minSize := config.AOFAutoRewritePercentage, config.AOFAutoRewriteMinSize
	policy, syncFile := config.AOFFsync, aofSync
	defer func() {
		CloseAOF()
		config.AOFAutoRewritePercentage, config.AOFAutoRewriteMinSize = pct, minSize
		config.AOFFsync, aofSync = policy, syncFile
	}()
	config.AOFAutoRewritePercentage = 100
	config.AOFAutoRewriteMinSize = 4096
	config.AOFFsync = config.FsyncEverySec

	path := filepath.Join(t.TempDir(), "auto.aof")
	ResetStores()
	require.NoError(t, OpenAOF(path))
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
		aofSync = func(f *os.File) error { <-blocked; return f.Sync() }
		aof.lastSync = time.Now().Add(-2 * time.Second)
	}
	for i := 0; i < 4000; i++ {
		run(t, "SET", "hot", strconv.Itoa(i))
		require.NoError(t, FlushAOF())
		waitForRewriteSync(t)
	}
	if delayedSync {
		require.True(t, RewriteActive(), "file replacement must wait for the sync worker")
		require.Less(t, rewrite.written, int64(4096), "waiting for sync must not repeatedly serialize the hot key")
		size, err := os.Stat(path)
		require.NoError(t, err)
		require.Greater(t, size.Size(), int64(64*1024), "the old log grows while sync holds its descriptor")
	}
	releaseSync()
	// A background everysec sync keeps the old descriptor alive and can delay
	// replacement past the final write. Check size only after the automatic
	// rewrite has completed, including its idle event-loop turns.
	pollAOFSync(true)
	require.NoError(t, FlushAOF())
	waitForRewriteSync(t)
	for i := 0; RewriteActive() && i < 100; i++ {
		pollAOFSync(true)
		require.NoError(t, FlushAOF())
		waitForRewriteSync(t)
	}
	require.False(t, RewriteActive(), "automatic rewrite must finish after sync completes")

	_, _, rewrites, _ := AOFStats()
	assert.Greater(t, rewrites, 0, "a log growing past the threshold must rewrite itself")

	size, err := os.Stat(path)
	assert.NoError(t, err)
	assert.Less(t, size.Size(), int64(64*1024),
		"and must stay small, rather than growing between rewrites forever")

	assert.NoError(t, CloseAOF())
	ResetStores()
	_, err = LoadAOF(path)
	assert.NoError(t, err)
	assert.EqualValues(t, "3999", run(t, "GET", "hot"))
}

// TestAutomaticRewriteCanBeTurnedOff.
func TestAutomaticRewriteCanBeTurnedOff(t *testing.T) {
	pct, minSize := config.AOFAutoRewritePercentage, config.AOFAutoRewriteMinSize
	defer func() {
		config.AOFAutoRewritePercentage, config.AOFAutoRewriteMinSize = pct, minSize
	}()
	config.AOFAutoRewritePercentage = 0
	config.AOFAutoRewriteMinSize = 1

	path := filepath.Join(t.TempDir(), "off.aof")
	ResetStores()
	assert.NoError(t, OpenAOF(path))
	for i := 0; i < 2000; i++ {
		run(t, "SET", "hot", strconv.Itoa(i))
		assert.NoError(t, FlushAOF())
		waitForRewriteSync(t)
	}
	_, _, rewrites, _ := AOFStats()
	assert.Equal(t, 0, rewrites, "zero percentage must disable automatic rewriting")
	assert.NoError(t, CloseAOF())
}

func TestRewriteWithoutAppendonlyIsAnError(t *testing.T) {
	ResetStores()
	assert.Error(t, RewriteAOF(), "there is nothing to rewrite when the log is off")
}

// TestRewriteStallProfile measures event-loop work separately from background
// sync waits. Snapshot traversal is incremental; filesystem calls and atomic
// value serialization can still exceed cooperative targets.
func TestRewriteStallProfile(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a million keys")
	}
	if raceEnabled {
		// This is a latency diagnostic. The separate slice-budget/replay test
		// covers correctness under race instrumentation.
		t.Skip("measures wall-clock latency, which -race inflates about fourfold")
	}
	path := filepath.Join(t.TempDir(), "profile.aof")
	ResetStores()
	assert.NoError(t, OpenAOF(path))
	const keys = 1000000
	for i := 0; i < keys; i++ {
		run(t, "SET", "key:"+strconv.Itoa(i), "value-of-some-length")
	}
	assert.NoError(t, FlushAOF())
	waitForRewriteSync(t)

	start := time.Now()
	assert.NoError(t, StartRewrite())
	collecting := time.Since(start)

	var walk []time.Duration
	var final, waiting time.Duration
	total := collecting
	for {
		waitStart := time.Now()
		for RewriteActive() && !RewriteNeedsCycle() {
			require.Less(t, time.Since(waitStart), 3*time.Second, "rewrite worker did not finish")
			time.Sleep(time.Millisecond)
		}
		waiting += time.Since(waitStart)
		t0 := time.Now()
		require.NoError(t, AdvanceRewrite())
		more := RewriteActive()
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

	// Shared-host scheduling and storage affect even the median. Correctness
	// uses work bounds; the scheduled-probe harness measures latency separately.
	assert.Greater(t, len(walk), 100, "the walk must be spread over many cycles")
	assert.Equal(t, 1, aof.rewrites, "the rewrite must actually commit")
	assert.NoError(t, CloseAOF())
}

// BenchmarkRewrite measures the stall a rewrite costs, which is the number the
// package comment has to be able to quote. It blocks the event loop, so what a
// client sees is this figure in full.
func BenchmarkRewrite(b *testing.B) {
	for _, keys := range []int{10000, 100000, 1000000} {
		b.Run(strconv.Itoa(keys)+"-string-keys", func(b *testing.B) {
			path := filepath.Join(b.TempDir(), "bench.aof")
			ResetStores()
			if err := OpenAOF(path); err != nil {
				b.Fatal(err)
			}
			defer CloseAOF()
			var w replyWriter
			for i := 0; i < keys; i++ {
				EvalAndResponse(&Command{Cmd: "SET",
					Args: []string{"key:" + strconv.Itoa(i), "value-of-some-length"}}, &w)
				w.b = w.b[:0]
			}
			if err := FlushAOF(); err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := RewriteAOF(); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(keys)*float64(b.N)/b.Elapsed().Seconds(), "keys/s")
		})
	}
}

// stepRewrite advances a rewrite by one slice, the way an event-loop cycle
// would, and reports whether it is still going.
func stepRewrite(t *testing.T) bool {
	t.Helper()
	assert.NoError(t, AdvanceRewrite())
	waitForRewriteSync(t)
	return RewriteActive()
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
	path := filepath.Join(t.TempDir(), "moving.aof")
	ResetStores()
	assert.NoError(t, OpenAOF(path))

	const keys = 20000
	for i := 0; i < keys; i++ {
		run(t, "SET", "k"+strconv.Itoa(i), "original")
	}
	assert.NoError(t, FlushAOF())
	waitForRewriteSync(t)
	assert.NoError(t, StartRewrite())

	// One slice, so the walk is part way through and has certainly recorded
	// some keys and not others.
	assert.True(t, stepRewrite(t), "the walk should need more than one slice")

	// Now move the ground under it, in every way that matters.
	run(t, "SET", "k0", "changed-after-the-walk-passed")
	run(t, "SET", "k19999", "changed-before-the-walk-arrived")
	run(t, "DEL", "k1")
	run(t, "SET", "brand-new", "created-mid-rewrite")
	run(t, "SADD", "new-set", "a", "b")
	run(t, "SET", "k2", "1")
	run(t, "INCR", "k2")
	run(t, "INCR", "k2")

	for stepRewrite(t) {
	}
	assert.NoError(t, CloseAOF())

	before := map[string]interface{}{
		"k0":        run(t, "GET", "k0"),
		"k2":        run(t, "GET", "k2"),
		"k19999":    run(t, "GET", "k19999"),
		"brand-new": run(t, "GET", "brand-new"),
		"new-set":   run(t, "SCARD", "new-set"),
		"count":     data_structure.TotalKeys(),
	}

	ResetStores()
	_, err := LoadAOF(path)
	assert.NoError(t, err)

	assert.Equal(t, before["k0"], run(t, "GET", "k0"), "a key written after the walk passed it")
	assert.Equal(t, before["k19999"], run(t, "GET", "k19999"), "a key written before the walk reached it")
	assert.Equal(t, before["brand-new"], run(t, "GET", "brand-new"), "a key created mid-rewrite")
	assert.Equal(t, before["new-set"], run(t, "SCARD", "new-set"), "a set created mid-rewrite")
	assert.Equal(t, before["count"], data_structure.TotalKeys(), "and no key resurrected or lost")

	// INCR is the one that catches a snapshot-plus-diff design applying an
	// operation twice: the value must be what it was, not what it would be if
	// the increments were replayed on top of a state that already had them.
	assert.EqualValues(t, "3", run(t, "GET", "k2"))
	assert.Equal(t, constant.RespNil, rawReply(t, "GET", "k1"), "a key deleted mid-rewrite stays deleted")
}

// TestRewriteOverridesASetRatherThanMergingWithIt.
//
// A set recorded by the walk and then changed would, without the DEL the
// override writes first, come back as the union of both versions - so members
// removed during the rewrite would be alive again after a restart.
func TestRewriteOverridesASetRatherThanMergingWithIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "merge.aof")
	ResetStores()
	assert.NoError(t, OpenAOF(path))
	run(t, "SADD", "s", "a", "b", "c", "d")
	// Padding, so the walk records the set and then has slices left to run.
	for i := 0; i < 5000; i++ {
		run(t, "SET", "pad"+strconv.Itoa(i), "v")
	}
	assert.NoError(t, FlushAOF())
	waitForRewriteSync(t)

	assert.NoError(t, StartRewrite())
	assert.True(t, stepRewrite(t))
	run(t, "SREM", "s", "a", "b")
	for stepRewrite(t) {
	}
	assert.NoError(t, CloseAOF())

	ResetStores()
	_, err := LoadAOF(path)
	assert.NoError(t, err)
	assert.EqualValues(t, 2, run(t, "SCARD", "s"),
		"members removed during a rewrite must not come back")
}

// TestRewriteDoesNotCountAsUse.
//
// The walk reads every key in the keyspace. If that counted as an access,
// eviction would come out of a rewrite believing everything was equally hot and
// with no idea what anyone had actually asked for.
func TestRewriteDoesNotCountAsUse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lru.aof")
	ResetStores()
	assert.NoError(t, OpenAOF(path))
	for i := 0; i < 100; i++ {
		run(t, "SET", "k"+strconv.Itoa(i), "v")
	}
	// One key is genuinely hot, and must still look hot afterwards.
	for i := 0; i < 50; i++ {
		run(t, "GET", "k7")
	}
	hotBefore, ok := dictStore.ScoreOf("k7")
	assert.True(t, ok)
	coldBefore, ok := dictStore.ScoreOf("k42")
	assert.True(t, ok)
	assert.Greater(t, hotBefore, coldBefore)

	assert.NoError(t, RewriteAOF())

	hotAfter, _ := dictStore.ScoreOf("k7")
	coldAfter, _ := dictStore.ScoreOf("k42")
	assert.Equal(t, hotBefore, hotAfter, "a rewrite must not touch a key's access record")
	assert.Equal(t, coldBefore, coldAfter)
	assert.Greater(t, hotAfter, coldAfter, "and must leave the hot key still looking hot")
	assert.NoError(t, CloseAOF())
}

// Verify bounded work and complete output independently of scheduler, GC and
// filesystem timing. Log durations as diagnostics; benchmarks measure latency.
func TestRewriteSlicesObeyKeyBudgetAndReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slice.aof")
	ResetStores()
	assert.NoError(t, OpenAOF(path))
	for i := 0; i < 200000; i++ {
		run(t, "SET", "k"+strconv.Itoa(i), "some value of a realistic length")
	}
	assert.NoError(t, FlushAOF())
	waitForRewriteSync(t)

	assert.NoError(t, StartRewrite())
	slices, worstWalk, final := 0, time.Duration(0), time.Duration(0)
	for {
		before := rewrite.pos
		start := time.Now()
		more := stepRewrite(t)
		took := time.Since(start)
		slices++
		assert.GreaterOrEqual(t, rewrite.pos-before, 0)
		assert.LessOrEqual(t, rewrite.pos-before, rewriteChunk,
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
	assert.Equal(t, 1, aof.rewrites, "the rewrite must commit rather than fall back to the original log")
	assert.NoError(t, CloseAOF())
	ResetStores()
	_, err := LoadAOF(path)
	assert.NoError(t, err)
	assert.Equal(t, 200000, dictStore.Len())
	for i := 0; i < 200000; i++ {
		assert.Equal(t, "some value of a realistic length", run(t, "GET", "k"+strconv.Itoa(i)))
	}
}

// TestCancelledRewriteLeavesTheOldLogIntact. A server stopping mid-rewrite must
// keep the log it has been appending to all along.
func TestCancelledRewriteLeavesTheOldLogIntact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cancel.aof")
	ResetStores()
	assert.NoError(t, OpenAOF(path))
	for i := 0; i < 10000; i++ {
		run(t, "SET", "k"+strconv.Itoa(i), "v")
	}
	assert.NoError(t, FlushAOF())
	waitForRewriteSync(t)

	assert.NoError(t, StartRewrite())
	assert.True(t, stepRewrite(t))
	CancelRewrite()
	assert.False(t, RewriteActive())

	run(t, "SET", "after-cancel", "v")
	assert.NoError(t, CloseAOF())

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), ".rewrite", "the abandoned file must be cleaned up")
	}

	ResetStores()
	_, err := LoadAOF(path)
	assert.NoError(t, err)
	assert.Equal(t, 10001, data_structure.TotalKeys(),
		"the old log must still hold everything, including writes after the cancellation")
}

// Reopening a log must not reset its growth threshold to accumulated history.
// Otherwise a restart every few minutes can defer automatic compaction forever.
func TestRestartDoesNotRatchetAutomaticRewriteBaseline(t *testing.T) {
	oldPct, oldMin := config.AOFAutoRewritePercentage, config.AOFAutoRewriteMinSize
	defer func() { CloseAOF(); config.AOFAutoRewritePercentage, config.AOFAutoRewriteMinSize = oldPct, oldMin }()
	config.AOFAutoRewritePercentage, config.AOFAutoRewriteMinSize = 100, 1024
	path := filepath.Join(t.TempDir(), "ratchet.aof")
	ResetStores()
	assert.NoError(t, OpenAOF(path))
	// Use flushAOF to create a replayable historical log without auto-compaction.
	for i := 0; i < 200; i++ {
		run(t, "SET", "hot", strconv.Itoa(i))
		assert.NoError(t, flushAOF(false))
	}
	assert.NoError(t, CloseAOF())
	ResetStores()
	_, err := LoadAOF(path)
	assert.NoError(t, err)
	assert.NoError(t, OpenAOF(path))
	assert.Greater(t, aof.baseSize, aof.rewriteBase)
	before := aof.baseSize
	for i := 0; i < 100 && aof.rewrites == 0; i++ {
		assert.NoError(t, FlushAOF())
		waitForRewriteSync(t)
	}
	assert.Equal(t, 1, aof.rewrites)
	assert.Less(t, aof.baseSize, before/4)
	assert.Equal(t, aof.baseSize, aof.rewriteBase)
	assert.NoError(t, CloseAOF())
	ResetStores()
	_, err = LoadAOF(path)
	assert.NoError(t, err)
	assert.Equal(t, "199", run(t, "GET", "hot"))
}

func TestRewriteStartDoesNotCopyKeyNames(t *testing.T) {
	for _, n := range []int{1000, 100000} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			ResetStores()
			require.NoError(t, OpenAOF(filepath.Join(t.TempDir(), "start.aof")))
			t.Cleanup(func() { CancelRewrite(); assert.NoError(t, CloseAOF()) })
			for i := 0; i < n; i++ {
				dictStore.Put("key:"+strconv.Itoa(i), dictStore.NewObj("v"))
			}
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			require.NoError(t, StartRewrite())
			runtime.ReadMemStats(&after)
			require.Empty(t, rewrite.keys, "start retains slot limits, not every name")
			require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(256<<10), "startup must not allocate an O(N) name slice")
			require.NoError(t, AdvanceRewrite())
			waitForRewriteSync(t)
			require.LessOrEqual(t, len(rewrite.keys), data_structure.ScanMaxWork)
			require.LessOrEqual(t, rewrite.pos, data_structure.ScanMaxWork)
		})
	}
}

func TestRewriteRetainsBoundedNameBatches(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a large keyspace")
	}
	ResetStores()
	assert.NoError(t, OpenAOF(filepath.Join(t.TempDir(), "held.aof")))
	defer func() { assert.NoError(t, CloseAOF()) }()

	const keys = 200000
	for i := 0; i < keys; i++ {
		run(t, "SET", "key:"+strconv.Itoa(i), "value-of-some-length")
	}
	assert.NoError(t, FlushAOF())
	waitForRewriteSync(t)
	assert.NoError(t, StartRewrite())

	// Nothing is collected up front, so the walk starts holding nothing at all.
	assert.Zero(t, len(rewrite.keys), "starting a rewrite must not enumerate the keyspace")

	worst := 0
	for stepRewrite(t) {
		worst = max(worst, cap(rewrite.keys))
	}
	assert.Equal(t, 1, aof.rewrites, "the rewrite must commit")

	// Slice capacity may round above the work limit, but never scales with N.
	assert.LessOrEqual(t, worst, 2*data_structure.ScanMaxWork,
		"retained batch capacity must follow the fixed traversal budget")

	t.Logf("%d keys: walk retained at most %d names, %d walked", keys, worst, rewrite.pos)
}

// A ceiling below the keyspace a server is allowed to hold is worse than none:
// auto-rewrite retries every minute, fails every time, and the log grows without
// bound. This checks the two numbers stay in a sane relation to each other and
// that refusing above the ceiling is a clean error rather than a started rewrite.
func TestRewriteCeilingCoversTheKeyspaceAServerMayHold(t *testing.T) {
	assert.LessOrEqual(t, rewriteKeyCeiling, config.KeyNumberLimit,
		"a ceiling above the key limit is dead configuration")
	assert.Greater(t, rewriteKeyCeiling, config.KeyNumberLimit/2,
		"a ceiling far below the key limit leaves legal keyspaces unable to compact, "+
			"which is how a log grows without bound")

	ResetStores()
	assert.NoError(t, OpenAOF(filepath.Join(t.TempDir(), "ceiling.aof")))
	defer func() { assert.NoError(t, CloseAOF()) }()
	run(t, "SET", "k", "v")

	// Above the ceiling the refusal is immediate and leaves nothing running, so
	// the caller can tell a refusal from an abort part way through.
	old := keyCountForRewrite
	defer func() { keyCountForRewrite = old }()
	keyCountForRewrite = func() int { return rewriteKeyCeiling + 1 }
	err := StartRewrite()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "rewrite limit")
	assert.False(t, RewriteActive(), "a refused rewrite must not leave one started")

	// At the ceiling it proceeds.
	keyCountForRewrite = func() int { return rewriteKeyCeiling }
	assert.NoError(t, StartRewrite())
	assert.True(t, RewriteActive())
	for stepRewrite(t) {
	}
	assert.Equal(t, 1, aof.rewrites)
}
