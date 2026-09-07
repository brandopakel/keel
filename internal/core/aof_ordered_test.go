package core

import (
	"bytes"
	"errors"
	"io"
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
