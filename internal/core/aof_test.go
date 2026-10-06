package core

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/brandopakel/keel/internal/constant"
)

// replyWriter captures what a command replied, so a test can both drive the
// real path and read the answer.
type replyWriter struct{ b []byte }

func (w *replyWriter) Read([]byte) (int, error) { return 0, io.EOF }
func (w *replyWriter) Write(p []byte) (int, error) {
	w.b = append(w.b, p...)
	return len(p), nil
}

// run executes a command the way a connection would.
//
// Calling cmdSET and friends directly skips EvalAndResponse, and that is where
// the log is written from - so a test that called them directly would drive the
// keyspace correctly and record none of it, then pass by observing that the
// keyspace was correct. Every command here goes the long way round for that
// reason.
func run(t *testing.T, name string, args ...string) interface{} {
	t.Helper()
	var w replyWriter
	if err := EvalAndResponse(&Command{Cmd: name, Args: args}, &w); err != nil {
		return err.Error()
	}
	res, _ := Decode(w.b)
	return res
}

// rawReply returns the encoded reply, for the cases where nil and empty differ.
func rawReply(t *testing.T, name string, args ...string) []byte {
	t.Helper()
	var w replyWriter
	if err := EvalAndResponse(&Command{Cmd: name, Args: args}, &w); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return w.b
}

// withAOF runs fn against a fresh keyspace with the log on, then closes it and
// returns the path, so a test can restart from it.
func withAOF(t *testing.T, fn func()) string {
	t.Helper()
	return withAOFOn(t, defaultEngine, fn)
}

// withAOFOn is withAOF on e.
func withAOFOn(t *testing.T, e *Engine, fn func()) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.aof")
	e.resetStores()
	assert.NoError(t, e.OpenAOF(path))
	fn()
	assert.NoError(t, e.FlushAOF())
	assert.NoError(t, e.CloseAOF())
	return path
}

// restart throws the keyspace away and rebuilds it from the log, which is what
// a real restart does.
func restart(t *testing.T, path string) int {
	t.Helper()
	return restartOn(t, defaultEngine, path)
}

// restartOn is restart on e.
func restartOn(t *testing.T, e *Engine, path string) int {
	t.Helper()
	e.resetStores()
	applied, err := e.LoadAOF(path)
	assert.NoError(t, err)
	return applied
}

func TestAOFRestoresEveryKeyspace(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := withAOFOn(t, e, func() {
		runOn(t, e, "SET", "str", "hello")
		runOn(t, e, "SET", "n", "41")
		runOn(t, e, "INCR", "n")
		runOn(t, e, "SADD", "set", "a", "b", "c")
		runOn(t, e, "ZADD", "z", "10", "alice")
		runOn(t, e, "PFADD", "hll", "x", "y", "z")
		runOn(t, e, "CF.RESERVE", "cf", "1000")
		runOn(t, e, "CF.ADD", "cf", "member")
		runOn(t, e, "BF.MADD", "bf", "member")
		runOn(t, e, "CMS.INITBYDIM", "cms", "100", "5")
		runOn(t, e, "CMS.INCRBY", "cms", "item", "7")
		runOn(t, e, "MORRIS.INITBYDIM", "mor", "200", "5")
		runOn(t, e, "MORRIS.INCRBY", "mor", "hits", "500000")
	})

	before := map[string]interface{}{
		"str":  runOn(t, e, "GET", "str"),
		"n":    runOn(t, e, "GET", "n"),
		"card": runOn(t, e, "SCARD", "set"),
		"z":    runOn(t, e, "ZSCORE", "z", "alice"),
		"hll":  runOn(t, e, "PFCOUNT", "hll"),
		"cf":   runOn(t, e, "CF.EXISTS", "cf", "member"),
		"bf":   runOn(t, e, "BF.EXISTS", "bf", "member"),
		"cms":  runOn(t, e, "CMS.QUERY", "cms", "item"),
		"mor":  runOn(t, e, "MORRIS.QUERY", "mor", "hits"),
	}

	restartOn(t, e, path)

	assert.Equal(t, before["str"], runOn(t, e, "GET", "str"))
	assert.Equal(t, before["n"], runOn(t, e, "GET", "n"), "INCR must not be lost or doubled")
	assert.Equal(t, before["card"], runOn(t, e, "SCARD", "set"))
	assert.Equal(t, before["z"], runOn(t, e, "ZSCORE", "z", "alice"))
	assert.Equal(t, before["hll"], runOn(t, e, "PFCOUNT", "hll"))
	assert.Equal(t, before["cf"], runOn(t, e, "CF.EXISTS", "cf", "member"))
	assert.Equal(t, before["bf"], runOn(t, e, "BF.EXISTS", "bf", "member"))
	assert.Equal(t, before["cms"], runOn(t, e, "CMS.QUERY", "cms", "item"))

	// The probabilistic types replay to the identical estimate rather than a
	// similar one, because every one of them seeds its randomness per
	// structure and from a constant. Replaying the same commands in the same
	// order therefore flips the same coins. If any of them ever moves to a
	// seed taken from the clock, this is the test that will notice.
	assert.Equal(t, before["mor"], runOn(t, e, "MORRIS.QUERY", "mor", "hits"),
		"a Morris counter must replay to the same estimate, not merely a close one")
}

// TestAOFDoesNotRecordReads. The log has to grow with changes, not with
// traffic, or a read-heavy server writes forever for no reason.
func TestAOFDoesNotRecordReads(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := withAOFOn(t, e, func() {
		runOn(t, e, "SET", "k", "v")
		for i := 0; i < 100; i++ {
			runOn(t, e, "GET", "k")
			runOn(t, e, "TTL", "k")
			runOn(t, e, "DBSIZE")
		}
	})
	data, err := os.ReadFile(path)
	assert.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(data), "SET"), "only the write belongs in the log")
	assert.NotContains(t, string(data), "GET")
}

// TestAOFDoesNotRecordFailedCommands.
func TestAOFDoesNotRecordFailedCommands(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := withAOFOn(t, e, func() {
		runOn(t, e, "SET", "only", "one")
		runOn(t, e, "SET", "bad")                     // wrong arity
		runOn(t, e, "CMS.INCRBY", "nosuch", "i", "1") // key does not exist
	})
	data, _ := os.ReadFile(path)
	assert.Equal(t, 1, strings.Count(string(data), "SET"))
	assert.NotContains(t, string(data), "CMS.INCRBY")
}

// TestAOFRecordsSpopAsTheRemovalItWas is the determinism case.
//
// SPOP takes members at random. Replaying the command would take different
// ones, so a set that was popped would hold different members after every
// restart - and the divergence is silent, because both sets are the right size.
func TestAOFRecordsSpopAsTheRemovalItWas(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	var remaining interface{}
	path := withAOFOn(t, e, func() {
		runOn(t, e, "SADD", "s", "a", "b", "c", "d", "e")
		runOn(t, e, "SPOP", "s", "2")
		remaining = runOn(t, e, "SMEMBERS", "s")
	})

	data, _ := os.ReadFile(path)
	assert.Contains(t, string(data), "SREM", "the log must record what was removed")
	assert.NotContains(t, string(data), "SPOP", "not the command that removed it")

	restartOn(t, e, path)
	assert.ElementsMatch(t, remaining, runOn(t, e, "SMEMBERS", "s"),
		"the same members must survive, not merely the same number of them")
}

// TestAOFRecordsExpiryAsAnInstant.
//
// EXPIRE means "from now", and a log replayed tomorrow has a different now. If
// the duration were recorded, every restart would renew every TTL and nothing
// with an expiry would ever actually expire.
func TestAOFRecordsExpiryAsAnInstant(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := withAOFOn(t, e, func() {
		runOn(t, e, "SET", "a", "v")
		runOn(t, e, "EXPIRE", "a", "100")
		runOn(t, e, "SET", "b", "v", "EX", "100")
	})

	data, _ := os.ReadFile(path)
	assert.Contains(t, string(data), "PEXPIREAT")
	assert.NotContains(t, string(data), "EXPIRE\r\n$1\r\na", "EXPIRE itself must not be replayed")

	restartOn(t, e, path)
	for _, key := range []string{"a", "b"} {
		ttl := runOn(t, e, "TTL", key)
		assert.InDelta(t, 100, ttl, 2, "%s must keep the expiry it had, not be granted a new one", key)
	}
}

// TestAOFRecordsExpiryThatHasAlreadyPassedAsRemoval. A key whose expiry falls
// due while the server is down must not come back to life on restart.
func TestAOFRecordsExpiryThatHasAlreadyPassedAsRemoval(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := filepath.Join(t.TempDir(), "past.aof")
	assert.NoError(t, e.OpenAOF(path))
	runOn(t, e, "SET", "ghost", "v")
	// An expiry a minute in the past, as one written before a long outage would
	// look by the time the server comes back.
	runOn(t, e, "PEXPIREAT", "ghost", strconv.FormatInt(time.Now().UnixMilli()-60_000, 10))
	assert.NoError(t, e.FlushAOF())
	assert.NoError(t, e.CloseAOF())

	restartOn(t, e, path)
	assert.Equal(t, constant.RespNil, rawReplyOn(t, e, "GET", "ghost"),
		"a key whose expiry passed while the server was down must stay gone")
}

// TestAOFRecordsEviction.
//
// Eviction has no command behind it, so a log that omits it replays into a
// keyspace holding keys the original had already dropped - which under the same
// memory bound then evicts a different set, so the two diverge further with
// every restart rather than converging.
func TestAOFRecordsEviction(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := filepath.Join(t.TempDir(), "evict.aof")
	assert.NoError(t, e.OpenAOF(path))
	reconfigure(t, e, func(o *Options) { o.MaxKeys = 20 })
	for i := 0; i < 200; i++ {
		runOn(t, e, "SET", "k"+strconv.Itoa(i), "v")
	}
	survived := e.space.TotalKeys()
	assert.NoError(t, e.FlushAOF())
	assert.NoError(t, e.CloseAOF())

	data, _ := os.ReadFile(path)
	assert.Contains(t, string(data), "DEL", "an evicted key must be recorded as removed")

	restartOn(t, e, path)
	assert.Equal(t, survived, e.space.TotalKeys(),
		"replay must not resurrect keys eviction had already dropped")
	assert.LessOrEqual(t, e.space.TotalKeys(), e.options.MaxKeys)
}

// TestAOFTruncatedTailIsRecoverable. A crash between two write syscalls leaves
// a partial command. Everything before it is good, and refusing to start over
// the last few bytes would turn a recoverable stop into a lost dataset.
func TestAOFTruncatedTailIsRecoverable(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := withAOFOn(t, e, func() {
		runOn(t, e, "SET", "a", "1")
		runOn(t, e, "SET", "b", "2")
		runOn(t, e, "SET", "c", "3")
	})

	data, _ := os.ReadFile(path)
	assert.NoError(t, os.WriteFile(path, data[:len(data)-6], 0o644))

	e.resetStores()
	applied, err := e.LoadAOF(path)
	assert.Error(t, err)
	assert.True(t, IsTruncatedAOF(err), "a half-written tail is recoverable, not corruption")
	assert.Equal(t, 2, applied, "the commands before the tear must still be applied")
	assert.EqualValues(t, "1", runOn(t, e, "GET", "a"))
	assert.EqualValues(t, "2", runOn(t, e, "GET", "b"))
}

// TestAOFMalformedIsNotTreatedAsTruncation. Garbage in the middle of the file
// is a real failure and has to be distinguishable from a torn tail, or a
// corrupted log would be silently loaded up to the corruption and no further.
func TestAOFMalformedIsNotTreatedAsTruncation(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := withAOFOn(t, e, func() { runOn(t, e, "SET", "a", "1") })

	data, _ := os.ReadFile(path)
	assert.NoError(t, os.WriteFile(path, append(data, []byte("this is not RESP\r\n")...), 0o644))

	e.resetStores()
	_, err := e.LoadAOF(path)
	assert.Error(t, err)
	assert.False(t, IsTruncatedAOF(err), "garbage is corruption, not a torn tail")
}

// TestAOFReplayDoesNotRewriteItself. Loading a log with recording still live
// would append every command it just read, doubling the file on every restart.
func TestAOFReplayDoesNotRewriteItself(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := withAOFOn(t, e, func() {
		for i := 0; i < 20; i++ {
			runOn(t, e, "SET", "k"+strconv.Itoa(i), "v")
		}
	})
	before, _ := os.Stat(path)

	e.resetStores()
	_, err := e.LoadAOF(path)
	assert.NoError(t, err)

	after, _ := os.Stat(path)
	assert.Equal(t, before.Size(), after.Size(), "replaying must not append to the log it is reading")
}

func TestAOFDisabledWritesNothing(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.False(t, e.AOFEnabled())
	runOn(t, e, "SET", "k", "v")
	assert.NoError(t, e.FlushAOF(), "flushing with no log open must be harmless")
}

// TestAOFRecordsExpiryReapedByARead is the case that hides most easily.
//
// A key whose TTL has passed is removed by whatever reads it next, so the
// removal happens during a GET - a command that records nothing of itself. If
// the log only wrote removals for write commands, this one would be lost, and
// the key would come back on restart carrying an expiry already in the past.
func TestAOFRecordsExpiryReapedByARead(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := filepath.Join(t.TempDir(), "reap.aof")
	assert.NoError(t, e.OpenAOF(path))

	runOn(t, e, "SET", "fleeting", "v")
	runOn(t, e, "PEXPIREAT", "fleeting", strconv.FormatInt(time.Now().UnixMilli()+40, 10))
	for time.Now().UnixMilli() < time.Now().UnixMilli()+1 {
		break
	}
	// Wait past the expiry, then read it: the read is what reaps it.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
	}
	assert.Equal(t, constant.RespNil, rawReplyOn(t, e, "GET", "fleeting"))
	assert.Equal(t, 0, e.space.TotalKeys(), "the read must have reaped it")

	assert.NoError(t, e.FlushAOF())
	assert.NoError(t, e.CloseAOF())

	data, _ := os.ReadFile(path)
	assert.Contains(t, string(data), "DEL",
		"a removal that happened during a read must still be recorded")

	restartOn(t, e, path)
	assert.Equal(t, 0, e.space.TotalKeys(),
		"an expired key must not come back on restart")
}
