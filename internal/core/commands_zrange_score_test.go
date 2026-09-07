package core

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestScoreRangesAndPriorityQueuePersistence(t *testing.T) {
	path := withAOF(t, func() {
		run(t, "ZADD", "jobs", "1", "a", "1", "b", "2", "c", "3", "d")
		run(t, "PEXPIRE", "jobs", "60000")
		require.Equal(t, int64(2), run(t, "ZCOUNT", "jobs", "(1", "+inf"))
		require.Equal(t, []interface{}{"c", "2"}, run(t, "ZRANGEBYSCORE", "jobs", "(1", "3", "WITHSCORES", "LIMIT", "0", "1"))
		require.Equal(t, []interface{}{"b", "a"}, run(t, "ZREVRANGEBYSCORE", "jobs", "1", "-inf"))
		require.Equal(t, []interface{}{}, run(t, "ZRANGEBYSCORE", "jobs", "-inf", "+inf", "LIMIT", "-1", "5"))
		require.Equal(t, "4", run(t, "ZINCRBY", "jobs", "3", "a"))
		require.Equal(t, []interface{}{"b", "1", "c", "2"}, run(t, "ZPOPMIN", "jobs", "2"))
		require.Equal(t, []interface{}{"a", "4"}, run(t, "ZPOPMAX", "jobs"))
		require.Greater(t, run(t, "PTTL", "jobs").(int64), int64(0))
	})
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotContains(t, string(body), "ZINCRBY")
	require.NotContains(t, string(body), "ZPOPMIN")
	require.NotContains(t, string(body), "ZPOPMAX")
	for i := 0; i < 2; i++ {
		restart(t, path)
		require.Equal(t, []interface{}{"d", "3"}, run(t, "ZRANGE", "jobs", "0", "-1", "WITHSCORES"))
	}
	require.Equal(t, []interface{}{"d", "3"}, run(t, "ZPOPMIN", "jobs", "999999"))
	require.Equal(t, int64(0), run(t, "EXISTS", "jobs"))
}

func TestScoreCommandErrorsLeaveDataUnchanged(t *testing.T) {
	ResetStores()
	run(t, "ZADD", "z", "inf", "a", "1", "b")
	want := run(t, "ZRANGE", "z", "0", "-1", "WITHSCORES")
	for _, parts := range [][]string{
		{"ZINCRBY", "z", "-inf", "a"}, {"ZINCRBY", "z", "nan", "b"},
		{"ZPOPMIN", "z", "-1"}, {"ZPOPMAX", "z", "x"},
		{"ZCOUNT", "z", "nan", "1"}, {"ZRANGEBYSCORE", "z", "0", "9", "LIMIT", "0"},
		{"ZRANGEBYSCORE", "z", "0", "9", "LIMIT", "bad", "1"},
	} {
		require.Equal(t, byte('-'), rawReply(t, parts[0], parts[1:]...)[0], parts)
		require.Equal(t, want, run(t, "ZRANGE", "z", "0", "-1", "WITHSCORES"))
	}
	run(t, "SET", "wrong", "v")
	for _, parts := range [][]string{{"ZCOUNT", "wrong", "0", "1"}, {"ZRANGEBYSCORE", "wrong", "0", "1"}, {"ZINCRBY", "wrong", "1", "a"}, {"ZPOPMAX", "wrong"}} {
		require.Contains(t, string(rawReply(t, parts[0], parts[1:]...)), "WRONGTYPE")
	}
}
