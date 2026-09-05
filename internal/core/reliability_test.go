package core

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/brandopakel/keel/internal/config"
	"github.com/brandopakel/keel/internal/data_structure"
	"github.com/stretchr/testify/require"
)

func TestUntrustedSizingAndRestore(t *testing.T) {
	ResetStores()
	for _, parts := range [][]string{
		{"MORRIS.INITBYDIM", "m", "4294967295", "4294967295"},
		{"MORRIS.INITBYPROB", "m", "NaN", "0.1"},
		{"MORRIS.INITBYPROB", "m", "1e-100", "0.1"},
		{"CF.RESERVE", "c", "18446744073709551615"},
	} {
		require.Equal(t, byte('-'), rawReply(t, parts[0], parts[1:]...)[0])
	}
	payload := []byte{dumpTagCuckoo}
	for _, n := range []uint64{1 << 61, 0, 0, 0, 1, 1 << 63} {
		payload = binary.LittleEndian.AppendUint64(payload, n)
	}
	run(t, "SET", "keep", "value")
	require.Error(t, restoreKey("keep", payload))
	require.Equal(t, "value", run(t, "GET", "keep"))
	require.Zero(t, data_structure.TotalKeys()-1)
}

func TestFailedMorrisBatchIsAtomicAcrossRestart(t *testing.T) {
	path := withAOF(t, func() {
		run(t, "MORRIS.INITBYDIM", "m", "100", "3")
		before, _ := dumpKey("m")
		require.Equal(t, byte('-'), rawReply(t, "MORRIS.INCRBY", "m", "a", "1", "b", "invalid")[0])
		after, _ := dumpKey("m")
		require.Equal(t, before, after)
	})
	restart(t, path)
	require.Equal(t, []interface{}{"0"}, run(t, "MORRIS.QUERY", "m", "a"))
}

func TestAccountingMutationAndExpiry(t *testing.T) {
	ResetStores()
	run(t, "SET", "n", "9")
	run(t, "INCR", "n")
	run(t, "DEL", "n")
	require.Zero(t, data_structure.TotalMemUsed())
	old := config.MaxMemory
	defer func() { config.MaxMemory = old; ResetStores() }()
	for _, budget := range []uint64{100, 120, 150} {
		ResetStores()
		config.MaxMemory = budget
		run(t, "SET", "k", "v", "EX", "60")
		require.LessOrEqual(t, data_structure.TotalMemUsed(), budget)
		if data_structure.TotalKeys() == 0 {
			require.Zero(t, KeysWithExpiry())
			require.Zero(t, data_structure.TotalMemUsed())
		}
	}
}

func TestAllTypesExpireAndSurviveRewrite(t *testing.T) {
	creators := [][]string{
		{"SET", "k", "v"}, {"HSET", "k", "f", "v"}, {"RPUSH", "k", "v"}, {"SADD", "k", "v"},
		{"ZADD", "k", "1", "v"}, {"BF.ADD", "k", "v"}, {"CMS.INITBYDIM", "k", "10", "3"},
		{"MORRIS.INITBYDIM", "k", "10", "3"}, {"PFADD", "k", "v"}, {"CF.ADD", "k", "v"},
	}
	for _, creator := range creators {
		t.Run(creator[0], func(t *testing.T) {
			path := withAOF(t, func() {
				run(t, creator[0], creator[1:]...)
				require.NotNil(t, run(t, "MEMORY", "USAGE", "k"))
				require.EqualValues(t, 1, run(t, "EXPIRE", "k", "3600", "NX"))
				require.EqualValues(t, 0, run(t, "EXPIRE", "k", "7200", "NX"))
				require.EqualValues(t, 1, run(t, "EXPIRE", "k", "7200", "GT"))
				require.NoError(t, RewriteAOF())
			})
			restart(t, path)
			require.Greater(t, run(t, "TTL", "k").(int64), int64(7000))
			require.EqualValues(t, 1, run(t, "PERSIST", "k"))
			require.EqualValues(t, -1, run(t, "PTTL", "k"))
			owner, _ := data_structure.OwnerOf("k")
			owner.SetExpiryAt("k", 1)
			for i := 0; i < 20 && data_structure.TotalKeys() > 0; i++ {
				ExpireCycle()
			}
			require.Zero(t, data_structure.TotalKeys())
			require.Zero(t, data_structure.TotalMemUsed())
		})
	}
}

func TestSETOptionsReplayEffects(t *testing.T) {
	until := strconv.FormatInt(time.Now().Add(time.Hour).UnixMilli(), 10)
	path := withAOF(t, func() {
		require.Equal(t, "OK", run(t, "SET", "k", "one", "NX", "PXAT", until))
		require.Equal(t, "one", run(t, "SET", "k", "two", "XX", "GET", "KEEPTTL"))
		require.Equal(t, "two", run(t, "SET", "k", "ignored", "NX", "GET"))
		require.Equal(t, "two", run(t, "GET", "k"))
		require.Greater(t, run(t, "TTL", "k").(int64), int64(3500))
	})
	restart(t, path)
	require.Equal(t, "two", run(t, "GET", "k"))
	require.Greater(t, run(t, "TTL", "k").(int64), int64(3500))
}

func TestTornTailRepairSurvivesSecondRestart(t *testing.T) {
	ResetStores()
	path := filepath.Join(t.TempDir(), "log.aof")
	body := appendCommand(nil, "SET", "before", "yes")
	body = append(body, []byte("*3\r\n$3\r\nSET\r\n$3\r\nkey\r\n$9\r\nx")...)
	require.NoError(t, os.WriteFile(path, body, 0600))
	_, err := LoadAOF(path)
	require.True(t, IsTruncatedAOF(err))
	require.NoError(t, RepairAOFTail(err))
	require.NoError(t, OpenAOF(path))
	run(t, "SET", "after", "yes")
	require.NoError(t, CloseAOF())
	ResetStores()
	_, err = LoadAOF(path)
	require.NoError(t, err)
	require.Equal(t, "yes", run(t, "GET", "before"))
	require.Equal(t, "yes", run(t, "GET", "after"))
}

func TestIdleFsyncAndStickyFailure(t *testing.T) {
	ResetStores()
	oldPolicy, oldSync := config.AOFFsync, aofSync
	defer func() { CloseAOF(); config.AOFFsync = oldPolicy; aofSync = oldSync }()
	config.AOFFsync = config.FsyncEverySec
	require.NoError(t, OpenAOF(filepath.Join(t.TempDir(), "log")))
	calls := 0
	aofSync = func(*os.File) error { calls++; return nil }
	run(t, "SET", "k", "v")
	require.NoError(t, FlushAOF())
	require.Zero(t, calls)
	aof.lastSync = time.Now().Add(-2 * time.Second)
	require.NoError(t, FlushAOF())
	require.Equal(t, 1, calls)
	require.False(t, aof.dirty)
	config.AOFFsync = config.FsyncAlways
	diskErr := errors.New("injected sync failure")
	aofSync = func(*os.File) error { return diskErr }
	run(t, "SET", "k", "next")
	require.ErrorIs(t, FlushAOF(), diskErr)
	aofSync = oldSync
	require.ErrorIs(t, FlushAOF(), diskErr)
}

func TestDumpCollectionsAndChecksum(t *testing.T) {
	ResetStores()
	for _, cmd := range [][]string{{"HSET", "h", "f", "v"}, {"RPUSH", "l", "a", "b"}} {
		run(t, cmd[0], cmd[1:]...)
		payload, _ := dumpKey(cmd[1])
		require.NoError(t, restoreKey("copy", payload))
		require.Equal(t, run(t, "TYPE", cmd[1]), run(t, "TYPE", "copy"))
		payload[len(payload)-1] ^= 1
		require.Error(t, restoreKey("copy", payload))
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
		_ = restoreKey("fuzz", p)
	})
}

func TestCollectionRangesAndTrimPersistence(t *testing.T) {
	path := withAOF(t, func() {
		run(t, "ZADD", "z", "1", "a", "1", "b", "2", "c")
		require.Equal(t, []interface{}{"c", "2", "b", "1"}, run(t, "ZRANGE", "z", "0", "1", "REV", "WITHSCORES"))
		require.Equal(t, []interface{}{"b", "c"}, run(t, "ZRANGE", "z", "-2", "-1"))
		run(t, "RPUSH", "l", "a", "b", "c", "d")
		run(t, "PEXPIRE", "l", "60000")
		run(t, "LTRIM", "l", "1", "-2")
		require.Equal(t, []interface{}{"b", "c"}, run(t, "LRANGE", "l", "0", "-1"))
	})
	restart(t, path)
	require.Equal(t, []interface{}{"b", "c"}, run(t, "LRANGE", "l", "0", "-1"))
	require.Greater(t, run(t, "PTTL", "l").(int64), int64(50000))
}

func TestSmallBloomDumpRoundTrip(t *testing.T) {
	ResetStores()
	run(t, "BF.RESERVE", "b", "0.9", "1")
	payload, ok := dumpKey("b")
	require.True(t, ok)
	require.NoError(t, restoreKey("copy", payload))
	run(t, "BF.ADD", "copy", "member")
	require.Equal(t, int64(1), run(t, "BF.EXISTS", "copy", "member"))
}

func TestExpirySamplingDoesNotStarveCollections(t *testing.T) {
	ResetStores()
	for i := 0; i < 100; i++ {
		key := strconv.Itoa(i)
		run(t, "SET", key, "v", "EX", "600")
	}
	run(t, "HSET", "h", "f", "v")
	hashStore.SetExpiryAt("h", 1)
	for i := 0; i < 20; i++ {
		ExpireCycle()
	}
	require.Zero(t, hashStore.Len())
}

func TestReplayExpiryDoesNotResurrectHistoricalMutations(t *testing.T) {
	path := withAOF(t, func() {
		run(t, "SET", "n", "9", "PX", "100")
		run(t, "INCR", "n")
		run(t, "HSET", "h", "a", "1")
		run(t, "PEXPIRE", "h", "100")
		run(t, "HSET", "h", "b", "2")
		run(t, "CMS.INITBYDIM", "cms", "10", "2")
		run(t, "PEXPIRE", "cms", "100")
		run(t, "CMS.INCRBY", "cms", "item", "1")
	})
	time.Sleep(120 * time.Millisecond)
	restart(t, path)
	require.Zero(t, data_structure.TotalKeys())
	require.Zero(t, data_structure.TotalMemUsed())
}

func TestStartupExpiryIsLoggedBeforeKeyReuse(t *testing.T) {
	path := withAOF(t, func() {
		run(t, "SET", "n", "9", "PX", "100")
		run(t, "HSET", "h", "old", "value")
		run(t, "PEXPIRE", "h", "100")
	})
	time.Sleep(120 * time.Millisecond)
	restart(t, path)
	require.Zero(t, data_structure.TotalKeys())
	require.NoError(t, OpenAOF(path))
	run(t, "INCR", "n")
	run(t, "HSET", "h", "new", "value")
	require.NoError(t, CloseAOF())
	restart(t, path)
	require.Equal(t, "1", run(t, "GET", "n"))
	require.Equal(t, int64(1), run(t, "HLEN", "h"))
	require.Equal(t, "value", run(t, "HGET", "h", "new"))
	require.Equal(t, int64(-1), run(t, "PTTL", "h"))
}

func TestStartupEvictionsRemainDeletedAfterBudgetIncrease(t *testing.T) {
	old := config.KeyNumberLimit
	defer func() { config.KeyNumberLimit = old; ResetStores() }()
	config.KeyNumberLimit = 100
	path := withAOF(t, func() { run(t, "SET", "a", "1"); run(t, "SET", "b", "2") })
	config.KeyNumberLimit = 1
	restart(t, path)
	require.Equal(t, 1, data_structure.TotalKeys())
	require.NoError(t, OpenAOF(path))
	require.NoError(t, CloseAOF())
	config.KeyNumberLimit = 100
	restart(t, path)
	require.Equal(t, 1, data_structure.TotalKeys())
}

func TestLazyExpiryIsLoggedBeforeRecreation(t *testing.T) {
	path := withAOF(t, func() {
		run(t, "SET", "n", "9", "PX", "100")
		run(t, "HSET", "h", "old", "value")
		run(t, "PEXPIRE", "h", "100")
		time.Sleep(120 * time.Millisecond)
		require.Equal(t, int64(1), run(t, "INCR", "n"))
		require.Equal(t, int64(1), run(t, "HSET", "h", "new", "value"))
	})
	restart(t, path)
	require.Equal(t, "1", run(t, "GET", "n"))
	require.Equal(t, int64(1), run(t, "HLEN", "h"))
	require.Equal(t, "value", run(t, "HGET", "h", "new"))
}
