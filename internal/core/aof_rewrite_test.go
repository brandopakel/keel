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
