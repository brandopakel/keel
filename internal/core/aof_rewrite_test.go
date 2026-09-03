package core

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"memkv/internal/config"
	"memkv/internal/constant"
	"memkv/internal/data_structure"
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
	pct, min := config.AOFAutoRewritePercentage, config.AOFAutoRewriteMinSize
	defer func() {
		config.AOFAutoRewritePercentage, config.AOFAutoRewriteMinSize = pct, min
	}()
	config.AOFAutoRewritePercentage = 100
	config.AOFAutoRewriteMinSize = 4096

	path := filepath.Join(t.TempDir(), "auto.aof")
	ResetStores()
	assert.NoError(t, OpenAOF(path))

	for i := 0; i < 4000; i++ {
		run(t, "SET", "hot", strconv.Itoa(i))
		assert.NoError(t, FlushAOF())
	}
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
	pct, min := config.AOFAutoRewritePercentage, config.AOFAutoRewriteMinSize
	defer func() {
		config.AOFAutoRewritePercentage, config.AOFAutoRewriteMinSize = pct, min
	}()
	config.AOFAutoRewritePercentage = 0
	config.AOFAutoRewriteMinSize = 1

	path := filepath.Join(t.TempDir(), "off.aof")
	ResetStores()
	assert.NoError(t, OpenAOF(path))
	for i := 0; i < 2000; i++ {
		run(t, "SET", "hot", strconv.Itoa(i))
		assert.NoError(t, FlushAOF())
	}
	_, _, rewrites, _ := AOFStats()
	assert.Equal(t, 0, rewrites, "zero percentage must disable automatic rewriting")
	assert.NoError(t, CloseAOF())
}

func TestRewriteWithoutAppendonlyIsAnError(t *testing.T) {
	ResetStores()
	assert.Error(t, RewriteAOF(), "there is nothing to rewrite when the log is off")
}

// TestRewriteStallProfile is the measurement the package comment quotes.
//
// A rewrite is three different stalls, and averaging them would hide the one
// that matters: collecting the key names at the start, the slices of the walk,
// and the final slice that writes what changed and syncs the file. Only the
// walk was made incremental, so the other two are what is left to improve.
func TestRewriteStallProfile(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a million keys")
	}
	if raceEnabled {
		// Measured on Linux: the median slice goes from 793µs to 3.58ms under
		// -race, which fails the 2ms bound below. That is the detector's
		// instrumentation, not the walk, so running it here measures the wrong
		// thing rather than measuring this one badly.
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

	start := time.Now()
	assert.NoError(t, StartRewrite())
	collecting := time.Since(start)

	var walk []time.Duration
	var final time.Duration
	total := collecting
	for {
		t0 := time.Now()
		more := stepRewrite(t)
		took := time.Since(t0)
		total += took
		if more {
			walk = append(walk, took)
			continue
		}
		final = took
		break
	}

	sortDurations(walk)
	median := walk[len(walk)/2]
	worst := walk[len(walk)-1]
	t.Logf("%d keys: collecting %v, %d walk slices median %v worst %v, final %v, total %v",
		keys, collecting.Round(time.Millisecond), len(walk),
		median.Round(time.Microsecond), worst.Round(time.Microsecond),
		final.Round(time.Millisecond), total.Round(time.Millisecond))

	// The median rather than the worst. A single slice can be caught by a
	// garbage collection of a million-key heap and take milliseconds through no
	// fault of the walk, and asserting on that measures the collector. What
	// this test is for is that the walk is sliced at all, which shows up as
	// hundreds of slices none of which is typically long.
	assert.Greater(t, len(walk), 100, "the walk must be spread over many cycles")
	assert.Less(t, median, 2*time.Millisecond, "a typical slice must be short")
	assert.Less(t, worst, total/4, "and no slice may be most of the rewrite")
	assert.NoError(t, CloseAOF())
}

// sortDurations is an insertion sort, which is plenty for a few hundred
// samples and avoids pulling in a comparison function for one call.
func sortDurations(d []time.Duration) {
	for i := 1; i < len(d); i++ {
		for j := i; j > 0 && d[j] < d[j-1]; j-- {
			d[j], d[j-1] = d[j-1], d[j]
		}
	}
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
				EvalAndResponse(&MemKVCmd{Cmd: "SET",
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

// TestRewriteSliceIsSmallEnoughToNotBeAStall. The point of the walk being
// incremental is that no single step is long, so the step is what gets measured
// rather than the whole rewrite.
func TestRewriteSliceIsSmallEnoughToNotBeAStall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slice.aof")
	ResetStores()
	assert.NoError(t, OpenAOF(path))
	for i := 0; i < 200000; i++ {
		run(t, "SET", "k"+strconv.Itoa(i), "some value of a realistic length")
	}
	assert.NoError(t, FlushAOF())

	assert.NoError(t, StartRewrite())
	slices, worstWalk, final := 0, time.Duration(0), time.Duration(0)
	for {
		start := time.Now()
		more := stepRewrite(t)
		took := time.Since(start)
		slices++
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
	assert.Less(t, worstWalk, 50*time.Millisecond,
		"and no slice may be anything like the whole rewrite")
	assert.NoError(t, CloseAOF())
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
