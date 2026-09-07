package core

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/brandopakel/keel/internal/config"
	"github.com/stretchr/testify/require"
)

func TestOrderedAppendExecutesWhilePausedAndGatesEachPrefix(t *testing.T) {
	for _, policy := range []string{config.FsyncNever, config.FsyncEverySec, config.FsyncAlways} {
		t.Run(policy, func(t *testing.T) {
			ResetStores()
			oldWrite, oldPolicy, oldAsync := aofWrite, config.AOFFsync, config.AOFAsyncAppend
			config.AOFFsync, config.AOFAsyncAppend = policy, true
			release := make(chan struct{})
			var once sync.Once
			defer func() {
				once.Do(func() { close(release) })
				CloseAOF()
				aofWrite, config.AOFFsync, config.AOFAsyncAppend = oldWrite, oldPolicy, oldAsync
			}()
			path := filepath.Join(t.TempDir(), "log")
			require.NoError(t, OpenAOF(path))
			entered := make(chan []byte, 2)
			aofWrite = func(f *os.File, body []byte) (int, error) {
				entered <- bytes.Clone(body)
				<-release
				return f.Write(body)
			}
			run(t, "SET", "k", "first")
			first := AppendOffset()
			ready, err := FlushAOFAsync(nil)
			require.NoError(t, err)
			require.False(t, ready)
			original := <-entered
			commands := []*Command{{Cmd: "SET", Args: []string{"k", "second"}}, {Cmd: "GET", Args: []string{"k"}}}
			reserved, replies, ok := AppendAdmission(commands)
			require.True(t, ok)
			require.True(t, AppendHasRoom(reserved))
			require.Positive(t, replies)
			run(t, "SET", "k", "second")
			require.Equal(t, "second", run(t, "GET", "k"), "execution proceeds while the first writer is explicitly paused")
			second := AppendOffset()
			require.Greater(t, second, first)
			require.Zero(t, AppendReadyOffset())
			require.LessOrEqual(t, len(aof.buf), reserved)
			require.Error(t, StartRewrite(), "a rewrite cannot switch file generations across a pending append")
			require.Equal(t, appendCommand(nil, "SET", "k", "first"), original, "worker input must remain an immutable prefix")
			once.Do(func() { close(release) })
			pollAppend(true)
			require.Equal(t, first, AppendReadyOffset(), "later replies remain gated")
			if policy == config.FsyncAlways {
				require.Equal(t, first, appendSynced)
			} else {
				require.Zero(t, appendSynced)
			}
			ready, err = FlushAOFAsync(nil)
			require.NoError(t, err)
			require.False(t, ready)
			pollAppend(true)
			require.Equal(t, second, AppendReadyOffset())
			require.NoError(t, CloseAOF())
			require.Equal(t, second, appendSynced)
			for i := 0; i < 2; i++ {
				ResetStores()
				_, err = LoadAOF(path)
				require.NoError(t, err)
				require.Equal(t, "second", run(t, "GET", "k"))
			}
		})
	}
}

func TestOrderedAppendFailuresNeverAdvanceReplyPrefix(t *testing.T) {
	for _, fault := range []string{"short", "write", "sync"} {
		t.Run(fault, func(t *testing.T) {
			ResetStores()
			oldWrite, oldSync, oldPolicy := aofWrite, aofSync, config.AOFFsync
			defer func() { CloseAOF(); aofWrite, aofSync, config.AOFFsync = oldWrite, oldSync, oldPolicy }()
			config.AOFFsync = config.FsyncAlways
			require.NoError(t, OpenAOF(filepath.Join(t.TempDir(), "log")))
			diskErr := errors.New("controlled disk failure")
			aofWrite = func(f *os.File, b []byte) (int, error) {
				if fault == "short" {
					return f.Write(b[:len(b)/2])
				}
				if fault == "write" {
					return 0, diskErr
				}
				return f.Write(b)
			}
			if fault == "sync" {
				aofSync = func(*os.File) error { return diskErr }
			}
			run(t, "SET", "first", "v")
			_, err := FlushAOFAsync(nil)
			require.NoError(t, err)
			run(t, "SET", "speculative", "v")
			pollAppend(true)
			require.Zero(t, AppendReadyOffset())
			require.Zero(t, appendSynced)
			if fault == "short" {
				require.ErrorIs(t, aof.failed, io.ErrShortWrite)
			} else {
				require.ErrorIs(t, aof.failed, diskErr)
			}
			_, err = FlushAOFAsync(nil)
			require.Error(t, err)
			require.False(t, AppendPending(), "failure must not start the queued suffix")
			require.Error(t, CloseAOF())
		})
	}
}

func TestAppendAdmissionBoundsGrowthRepliesAndCanonicalExpiry(t *testing.T) {
	ResetStores()
	oldMemory, oldKeys := config.MaxMemory, config.KeyNumberLimit
	defer func() { CloseAOF(); config.MaxMemory, config.KeyNumberLimit = oldMemory, oldKeys }()
	config.MaxMemory, config.KeyNumberLimit = 0, 100000
	require.NoError(t, OpenAOF(filepath.Join(t.TempDir(), "log")))
	for _, parts := range [][]string{
		{"SET", "k", strings.Repeat("v", 4096), "PX", "60000", "GET"},
		{"GET", "k"}, {"MGET", "k", "missing"}, {"SETEX", "k", "600", "new"},
		{"PSETEX", "k", "60000", "0"}, {"INCRBY", "k", "19"}, {"DECRBY", "k", "2"},
		{"MSET", "a", "1", "b", "2"}, {"EXISTS", "a", "b"}, {"TTL", "k"},
	} {
		cmd := &Command{Cmd: parts[0], Args: parts[1:]}
		logBound, replyBound, ok := AppendAdmission([]*Command{cmd})
		require.True(t, ok, parts)
		before := len(aof.buf)
		var out replicationReply
		require.NoError(t, EvalAndResponse(cmd, &out))
		require.LessOrEqual(t, len(aof.buf)-before, logBound, parts)
		require.LessOrEqual(t, len(out), replyBound, parts)
	}
	_, _, ok := AppendAdmission([]*Command{{Cmd: "SPOP", Args: []string{"set", "1000"}}})
	require.False(t, ok)
	config.KeyNumberLimit = 1
	_, _, ok = AppendAdmission([]*Command{{Cmd: "SET", Args: []string{"new", "v"}}})
	require.False(t, ok, "eviction requires the drained path")
	config.KeyNumberLimit = 100000
	config.MaxMemory = 1
	_, _, ok = AppendAdmission([]*Command{{Cmd: "GET", Args: []string{"a"}}})
	require.False(t, ok, "even a read enforces an already exceeded memory limit")
	require.False(t, AppendHasRoom(maxAsyncAppendBytes))
	require.False(t, AppendHasRoom(-1))
}

// admissionRun admits a whole run, executes it, and reports what it actually
// cost. The bound is for the run rather than for each command, so the run is
// what has to be measured against it.
func admissionRun(t *testing.T, parts ...[]string) (logBound, replyBound, logUsed, replyUsed int, admitted bool) {
	t.Helper()
	commands := make([]*Command, 0, len(parts))
	for _, p := range parts {
		commands = append(commands, &Command{Cmd: p[0], Args: p[1:]})
	}
	logBound, replyBound, admitted = AppendAdmission(commands)
	if !admitted {
		return 0, 0, 0, 0, false
	}
	before := len(aof.buf)
	for _, cmd := range commands {
		var out replicationReply
		require.NoError(t, EvalAndResponse(cmd, &out))
		replyUsed += len(out)
	}
	return logBound, replyBound, len(aof.buf) - before, replyUsed, true
}

func TestAppendAdmissionBoundsCollectionCommands(t *testing.T) {
	ResetStores()
	oldMemory, oldKeys := config.MaxMemory, config.KeyNumberLimit
	defer func() { CloseAOF(); config.MaxMemory, config.KeyNumberLimit = oldMemory, oldKeys }()
	config.MaxMemory, config.KeyNumberLimit = 0, 100000
	require.NoError(t, OpenAOF(filepath.Join(t.TempDir(), "log")))

	value := strings.Repeat("v", 512)
	for _, run := range [][][]string{
		{{"HSET", "h", "f1", value, "f2", value}},
		{{"HGET", "h", "f1"}}, {{"HMGET", "h", "f1", "f2"}}, {{"HGETALL", "h"}},
		{{"HKEYS", "h"}}, {{"HVALS", "h"}}, {{"HLEN", "h"}}, {{"HEXISTS", "h", "f1"}},
		{{"HDEL", "h", "f2"}},
		{{"SADD", "s", value, value + "b"}}, {{"SMEMBERS", "s"}}, {{"SCARD", "s"}},
		{{"SISMEMBER", "s", value}}, {{"SREM", "s", value}},
		{{"RPUSH", "l", value, value}}, {{"LPUSH", "l", value}}, {{"LRANGE", "l", "0", "-1"}},
		{{"LLEN", "l"}}, {{"LINDEX", "l", "0"}},
		{{"ZADD", "z", "1", value, "2", value + "b"}}, {{"ZRANGE", "z", "0", "-1"}},
		{{"ZRANGE", "z", "0", "-1", "WITHSCORES"}}, {{"ZCARD", "z"}},
		{{"ZSCORE", "z", value}}, {{"ZREM", "z", value}},
	} {
		logBound, replyBound, logUsed, replyUsed, ok := admissionRun(t, run...)
		require.True(t, ok, "%v must be admitted", run)
		require.LessOrEqual(t, logUsed, logBound, "%v transcript", run)
		require.LessOrEqual(t, replyUsed, replyBound, "%v reply", run)
	}
}

// A collection read later in a run can return everything written to a
// collection earlier in it, because none of the run has executed when the bound
// is computed. This is the case a per-command largest-write bound gets wrong.
func TestAppendAdmissionBoundsReadsOfCollectionsGrownInTheSameRun(t *testing.T) {
	ResetStores()
	oldMemory, oldKeys := config.MaxMemory, config.KeyNumberLimit
	defer func() { CloseAOF(); config.MaxMemory, config.KeyNumberLimit = oldMemory, oldKeys }()
	config.MaxMemory, config.KeyNumberLimit = 0, 100000
	require.NoError(t, OpenAOF(filepath.Join(t.TempDir(), "log")))

	big := strings.Repeat("x", 4096)
	for _, run := range [][][]string{
		{{"RPUSH", "grow", big, big, big, big}, {"LRANGE", "grow", "0", "-1"}},
		{{"SADD", "gset", big, big + "b", big + "c"}, {"SMEMBERS", "gset"}},
		{{"HSET", "ghash", "a", big, "b", big}, {"HGETALL", "ghash"}},
		{{"ZADD", "gz", "1", big, "2", big + "b"}, {"ZRANGE", "gz", "0", "-1", "WITHSCORES"}},
	} {
		logBound, replyBound, logUsed, replyUsed, ok := admissionRun(t, run...)
		require.True(t, ok, "%v must be admitted", run)
		require.LessOrEqual(t, logUsed, logBound, "%v transcript", run)
		require.LessOrEqual(t, replyUsed, replyBound,
			"%v: a read must be bounded by what the run can have written to it", run)
	}
}

// Commands whose log record only execution can produce must not be admitted.
func TestAppendAdmissionRefusesRecordsItCannotPredict(t *testing.T) {
	ResetStores()
	defer CloseAOF()
	require.NoError(t, OpenAOF(filepath.Join(t.TempDir(), "log")))
	for _, parts := range [][]string{
		{"SPOP", "s", "10"},              // stages SREM of whichever members it drew
		{"ZPOPMIN", "z"},                 // likewise
		{"LPOP", "l", "5"},               // may empty the key and delete it
		{"RPOP", "l"},                    //
		{"SRANDMEMBER", "s", "-1000000"}, // a negative count repeats members
		{"ZADD", "z", "NX", "1", "m"},    // flags change which members land
		{"ZADD", "z", "GT", "1", "m"},
		{"ZINCRBY", "z", "1", "m"}, // stages ZADD with the resulting score
		{"EXPIRE", "k", "1"},       // stages PEXPIREAT or an outright DEL
		{"FLUSHDB"},
		{"BF.ADD", "f", "x"},
		{"KEEL.RESTORE", "k", "payload"},
	} {
		_, _, ok := AppendAdmission([]*Command{{Cmd: parts[0], Args: parts[1:]}})
		require.False(t, ok, "%v must take the drained barrier", parts)
	}
}

// The hand-written cases above are the ones someone thought of. This one is the
// ones nobody did: random runs over the admitted command set, checking the only
// property that matters - what a run actually costs never exceeds what it was
// admitted for. A bound that is too generous only costs a barrier; a bound that
// is too tight is a budget already spent when the overrun is discovered.
func TestAppendAdmissionBoundHoldsForRandomRuns(t *testing.T) {
	ResetStores()
	oldMemory, oldKeys := config.MaxMemory, config.KeyNumberLimit
	defer func() { CloseAOF(); config.MaxMemory, config.KeyNumberLimit = oldMemory, oldKeys }()
	config.MaxMemory, config.KeyNumberLimit = 0, 1000000
	require.NoError(t, OpenAOF(filepath.Join(t.TempDir(), "log")))

	rng := rand.New(rand.NewSource(20260907))
	keys := []string{"a", "b", "c"}
	arg := func() string { return strings.Repeat("x", 1+rng.Intn(2048)) }
	key := func() string { return keys[rng.Intn(len(keys))] }

	shapes := []func() []string{
		func() []string { return []string{"SET", "s" + key(), arg()} },
		func() []string { return []string{"GET", "s" + key()} },
		func() []string { return []string{"MSET", "s" + key(), arg(), "s" + key(), arg()} },
		func() []string { return []string{"INCRBY", "n" + key(), "3"} },
		func() []string { return []string{"HSET", "h" + key(), arg(), arg()} },
		func() []string { return []string{"HGETALL", "h" + key()} },
		func() []string { return []string{"HVALS", "h" + key()} },
		func() []string { return []string{"SADD", "t" + key(), arg(), arg()} },
		func() []string { return []string{"SMEMBERS", "t" + key()} },
		func() []string { return []string{"RPUSH", "l" + key(), arg(), arg()} },
		func() []string { return []string{"LPUSH", "l" + key(), arg()} },
		func() []string { return []string{"LRANGE", "l" + key(), "0", "-1"} },
		func() []string { return []string{"ZADD", "z" + key(), "1", arg()} },
		func() []string { return []string{"ZRANGE", "z" + key(), "0", "-1", "WITHSCORES"} },
		func() []string { return []string{"HDEL", "h" + key(), arg()} },
		func() []string { return []string{"SREM", "t" + key(), arg()} },
	}

	admitted := 0
	for round := 0; round < 400; round++ {
		run := make([][]string, 0, 6)
		for i := 0; i < 1+rng.Intn(5); i++ {
			run = append(run, shapes[rng.Intn(len(shapes))]())
		}
		logBound, replyBound, logUsed, replyUsed, ok := admissionRun(t, run...)
		if !ok {
			continue
		}
		admitted++
		require.LessOrEqual(t, logUsed, logBound, "round %d transcript: %v", round, run)
		require.LessOrEqual(t, replyUsed, replyBound, "round %d reply: %v", round, run)
	}
	// If almost nothing were admitted the property above would be vacuous.
	require.Greater(t, admitted, 300, "the admitted set must actually be exercised")
	t.Logf("%d of 400 random runs admitted", admitted)
}
