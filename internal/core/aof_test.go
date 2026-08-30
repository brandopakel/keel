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

	"memkv/internal/config"
	"memkv/internal/constant"
	"memkv/internal/data_structure"
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
	if err := EvalAndResponse(&MemKVCmd{Cmd: name, Args: args}, &w); err != nil {
		return err.Error()
	}
	res, _ := Decode(w.b)
	return res
}

// rawReply returns the encoded reply, for the cases where nil and empty differ.
func rawReply(t *testing.T, name string, args ...string) []byte {
	t.Helper()
	var w replyWriter
	if err := EvalAndResponse(&MemKVCmd{Cmd: name, Args: args}, &w); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return w.b
}

// withAOF runs fn against a fresh keyspace with the log on, then closes it and
// returns the path, so a test can restart from it.
func withAOF(t *testing.T, fn func()) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.aof")
	ResetStores()
	assert.NoError(t, OpenAOF(path))
	fn()
	assert.NoError(t, FlushAOF())
	assert.NoError(t, CloseAOF())
	return path
}

// restart throws the keyspace away and rebuilds it from the log, which is what
// a real restart does.
func restart(t *testing.T, path string) int {
	t.Helper()
	ResetStores()
	applied, err := LoadAOF(path)
	assert.NoError(t, err)
	return applied
}

func TestAOFRestoresEveryKeyspace(t *testing.T) {
	path := withAOF(t, func() {
		run(t, "SET", "str", "hello")
		run(t, "SET", "n", "41")
		run(t, "INCR", "n")
		run(t, "SADD", "set", "a", "b", "c")
		run(t, "ZADD", "z", "10", "alice")
		run(t, "PFADD", "hll", "x", "y", "z")
		run(t, "CF.RESERVE", "cf", "1000")
		run(t, "CF.ADD", "cf", "member")
		run(t, "BF.MADD", "bf", "member")
		run(t, "CMS.INITBYDIM", "cms", "100", "5")
		run(t, "CMS.INCRBY", "cms", "item", "7")
		run(t, "MORRIS.INITBYDIM", "mor", "200", "5")
		run(t, "MORRIS.INCRBY", "mor", "hits", "500000")
	})

	before := map[string]interface{}{
		"str":  run(t, "GET", "str"),
		"n":    run(t, "GET", "n"),
		"card": run(t, "SCARD", "set"),
		"z":    run(t, "ZSCORE", "z", "alice"),
		"hll":  run(t, "PFCOUNT", "hll"),
		"cf":   run(t, "CF.EXISTS", "cf", "member"),
		"bf":   run(t, "BF.EXISTS", "bf", "member"),
		"cms":  run(t, "CMS.QUERY", "cms", "item"),
		"mor":  run(t, "MORRIS.QUERY", "mor", "hits"),
	}

	restart(t, path)

	assert.Equal(t, before["str"], run(t, "GET", "str"))
	assert.Equal(t, before["n"], run(t, "GET", "n"), "INCR must not be lost or doubled")
	assert.Equal(t, before["card"], run(t, "SCARD", "set"))
	assert.Equal(t, before["z"], run(t, "ZSCORE", "z", "alice"))
	assert.Equal(t, before["hll"], run(t, "PFCOUNT", "hll"))
	assert.Equal(t, before["cf"], run(t, "CF.EXISTS", "cf", "member"))
	assert.Equal(t, before["bf"], run(t, "BF.EXISTS", "bf", "member"))
	assert.Equal(t, before["cms"], run(t, "CMS.QUERY", "cms", "item"))

	// The probabilistic types replay to the identical estimate rather than a
	// similar one, because every one of them seeds its randomness per
	// structure and from a constant. Replaying the same commands in the same
	// order therefore flips the same coins. If any of them ever moves to a
	// seed taken from the clock, this is the test that will notice.
	assert.Equal(t, before["mor"], run(t, "MORRIS.QUERY", "mor", "hits"),
		"a Morris counter must replay to the same estimate, not merely a close one")
}

// TestAOFDoesNotRecordReads. The log has to grow with changes, not with
// traffic, or a read-heavy server writes forever for no reason.
func TestAOFDoesNotRecordReads(t *testing.T) {
	path := withAOF(t, func() {
		run(t, "SET", "k", "v")
		for i := 0; i < 100; i++ {
			run(t, "GET", "k")
			run(t, "TTL", "k")
			run(t, "DBSIZE")
		}
	})
	data, err := os.ReadFile(path)
	assert.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(data), "SET"), "only the write belongs in the log")
	assert.NotContains(t, string(data), "GET")
}

// TestAOFDoesNotRecordFailedCommands.
func TestAOFDoesNotRecordFailedCommands(t *testing.T) {
	path := withAOF(t, func() {
		run(t, "SET", "only", "one")
		run(t, "SET", "bad")                     // wrong arity
		run(t, "CMS.INCRBY", "nosuch", "i", "1") // key does not exist
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
	var remaining interface{}
	path := withAOF(t, func() {
		run(t, "SADD", "s", "a", "b", "c", "d", "e")
		run(t, "SPOP", "s", "2")
		remaining = run(t, "SMEMBERS", "s")
	})

	data, _ := os.ReadFile(path)
	assert.Contains(t, string(data), "SREM", "the log must record what was removed")
	assert.NotContains(t, string(data), "SPOP", "not the command that removed it")

	restart(t, path)
	assert.ElementsMatch(t, remaining, run(t, "SMEMBERS", "s"),
		"the same members must survive, not merely the same number of them")
}

// TestAOFRecordsExpiryAsAnInstant.
//
// EXPIRE means "from now", and a log replayed tomorrow has a different now. If
// the duration were recorded, every restart would renew every TTL and nothing
// with an expiry would ever actually expire.
func TestAOFRecordsExpiryAsAnInstant(t *testing.T) {
	path := withAOF(t, func() {
		run(t, "SET", "a", "v")
		run(t, "EXPIRE", "a", "100")
		run(t, "SET", "b", "v", "EX", "100")
	})

	data, _ := os.ReadFile(path)
	assert.Contains(t, string(data), "PEXPIREAT")
	assert.NotContains(t, string(data), "EXPIRE\r\n$1\r\na", "EXPIRE itself must not be replayed")

	restart(t, path)
	for _, key := range []string{"a", "b"} {
		ttl := run(t, "TTL", key)
		assert.InDelta(t, 100, ttl, 2, "%s must keep the expiry it had, not be granted a new one", key)
	}
}

// TestAOFRecordsExpiryThatHasAlreadyPassedAsRemoval. A key whose expiry falls
// due while the server is down must not come back to life on restart.
func TestAOFRecordsExpiryThatHasAlreadyPassedAsRemoval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "past.aof")
	ResetStores()
	assert.NoError(t, OpenAOF(path))
	run(t, "SET", "ghost", "v")
	// An expiry a minute in the past, as one written before a long outage would
	// look by the time the server comes back.
	run(t, "PEXPIREAT", "ghost", strconv.FormatInt(time.Now().UnixMilli()-60_000, 10))
	assert.NoError(t, FlushAOF())
	assert.NoError(t, CloseAOF())

	restart(t, path)
	assert.Equal(t, constant.RespNil, rawReply(t, "GET", "ghost"),
		"a key whose expiry passed while the server was down must stay gone")
}

// TestAOFRecordsEviction.
//
// Eviction has no command behind it, so a log that omits it replays into a
// keyspace holding keys the original had already dropped - which under the same
// memory bound then evicts a different set, so the two diverge further with
// every restart rather than converging.
func TestAOFRecordsEviction(t *testing.T) {
	originalKeys := config.KeyNumberLimit
	defer func() { config.KeyNumberLimit = originalKeys }()

	path := filepath.Join(t.TempDir(), "evict.aof")
	ResetStores()
	assert.NoError(t, OpenAOF(path))
	config.KeyNumberLimit = 20
	for i := 0; i < 200; i++ {
		run(t, "SET", "k"+strconv.Itoa(i), "v")
	}
	survived := data_structure.TotalKeys()
	assert.NoError(t, FlushAOF())
	assert.NoError(t, CloseAOF())

	data, _ := os.ReadFile(path)
	assert.Contains(t, string(data), "DEL", "an evicted key must be recorded as removed")

	restart(t, path)
	assert.Equal(t, survived, data_structure.TotalKeys(),
		"replay must not resurrect keys eviction had already dropped")
	assert.LessOrEqual(t, data_structure.TotalKeys(), config.KeyNumberLimit)
}

// TestAOFTruncatedTailIsRecoverable. A crash between two write syscalls leaves
// a partial command. Everything before it is good, and refusing to start over
// the last few bytes would turn a recoverable stop into a lost dataset.
func TestAOFTruncatedTailIsRecoverable(t *testing.T) {
	path := withAOF(t, func() {
		run(t, "SET", "a", "1")
		run(t, "SET", "b", "2")
		run(t, "SET", "c", "3")
	})

	data, _ := os.ReadFile(path)
	assert.NoError(t, os.WriteFile(path, data[:len(data)-6], 0o644))

	ResetStores()
	applied, err := LoadAOF(path)
	assert.Error(t, err)
	assert.True(t, IsTruncatedAOF(err), "a half-written tail is recoverable, not corruption")
	assert.Equal(t, 2, applied, "the commands before the tear must still be applied")
	assert.EqualValues(t, "1", run(t, "GET", "a"))
	assert.EqualValues(t, "2", run(t, "GET", "b"))
}

// TestAOFMalformedIsNotTreatedAsTruncation. Garbage in the middle of the file
// is a real failure and has to be distinguishable from a torn tail, or a
// corrupted log would be silently loaded up to the corruption and no further.
func TestAOFMalformedIsNotTreatedAsTruncation(t *testing.T) {
	path := withAOF(t, func() { run(t, "SET", "a", "1") })

	data, _ := os.ReadFile(path)
	assert.NoError(t, os.WriteFile(path, append(data, []byte("this is not RESP\r\n")...), 0o644))

	ResetStores()
	_, err := LoadAOF(path)
	assert.Error(t, err)
	assert.False(t, IsTruncatedAOF(err), "garbage is corruption, not a torn tail")
}

// TestAOFReplayDoesNotRewriteItself. Loading a log with recording still live
// would append every command it just read, doubling the file on every restart.
func TestAOFReplayDoesNotRewriteItself(t *testing.T) {
	path := withAOF(t, func() {
		for i := 0; i < 20; i++ {
			run(t, "SET", "k"+strconv.Itoa(i), "v")
		}
	})
	before, _ := os.Stat(path)

	ResetStores()
	_, err := LoadAOF(path)
	assert.NoError(t, err)

	after, _ := os.Stat(path)
	assert.Equal(t, before.Size(), after.Size(), "replaying must not append to the log it is reading")
}

func TestAOFDisabledWritesNothing(t *testing.T) {
	ResetStores()
	assert.False(t, AOFEnabled())
	run(t, "SET", "k", "v")
	assert.NoError(t, FlushAOF(), "flushing with no log open must be harmless")
}

// TestAOFRecordsExpiryReapedByARead is the case that hides most easily.
//
// A key whose TTL has passed is removed by whatever reads it next, so the
// removal happens during a GET - a command that records nothing of itself. If
// the log only wrote removals for write commands, this one would be lost, and
// the key would come back on restart carrying an expiry already in the past.
func TestAOFRecordsExpiryReapedByARead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reap.aof")
	ResetStores()
	assert.NoError(t, OpenAOF(path))

	run(t, "SET", "fleeting", "v")
	run(t, "PEXPIREAT", "fleeting", strconv.FormatInt(time.Now().UnixMilli()+40, 10))
	for time.Now().UnixMilli() < time.Now().UnixMilli()+1 {
		break
	}
	// Wait past the expiry, then read it: the read is what reaps it.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
	}
	assert.Equal(t, constant.RespNil, rawReply(t, "GET", "fleeting"))
	assert.Equal(t, 0, data_structure.TotalKeys(), "the read must have reaped it")

	assert.NoError(t, FlushAOF())
	assert.NoError(t, CloseAOF())

	data, _ := os.ReadFile(path)
	assert.Contains(t, string(data), "DEL",
		"a removal that happened during a read must still be recorded")

	restart(t, path)
	assert.Equal(t, 0, data_structure.TotalKeys(),
		"an expired key must not come back on restart")
}
