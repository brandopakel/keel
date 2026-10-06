package core

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestScoreRangesAndPriorityQueuePersistence(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	path := withAOFOn(t, e, func() {
		runOn(t, e, "ZADD", "jobs", "1", "a", "1", "b", "2", "c", "3", "d")
		runOn(t, e, "PEXPIRE", "jobs", "60000")
		require.Equal(t, int64(2), runOn(t, e, "ZCOUNT", "jobs", "(1", "+inf"))
		require.Equal(t, []interface{}{"c", "2"}, runOn(t, e, "ZRANGEBYSCORE", "jobs", "(1", "3", "WITHSCORES", "LIMIT", "0", "1"))
		require.Equal(t, []interface{}{"b", "a"}, runOn(t, e, "ZREVRANGEBYSCORE", "jobs", "1", "-inf"))
		require.Equal(t, []interface{}{}, runOn(t, e, "ZRANGEBYSCORE", "jobs", "-inf", "+inf", "LIMIT", "-1", "5"))
		require.Equal(t, "4", runOn(t, e, "ZINCRBY", "jobs", "3", "a"))
		require.Equal(t, []interface{}{"b", "1", "c", "2"}, runOn(t, e, "ZPOPMIN", "jobs", "2"))
		require.Equal(t, []interface{}{"a", "4"}, runOn(t, e, "ZPOPMAX", "jobs"))
		require.Greater(t, runOn(t, e, "PTTL", "jobs").(int64), int64(0))
	})
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotContains(t, string(body), "ZINCRBY")
	require.NotContains(t, string(body), "ZPOPMIN")
	require.NotContains(t, string(body), "ZPOPMAX")
	for i := 0; i < 2; i++ {
		restartOn(t, e, path)
		require.Equal(t, []interface{}{"d", "3"}, runOn(t, e, "ZRANGE", "jobs", "0", "-1", "WITHSCORES"))
	}
	require.Equal(t, []interface{}{"d", "3"}, runOn(t, e, "ZPOPMIN", "jobs", "999999"))
	require.Equal(t, int64(0), runOn(t, e, "EXISTS", "jobs"))
}

func TestScoreCommandErrorsLeaveDataUnchanged(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	runOn(t, e, "ZADD", "z", "inf", "a", "1", "b")
	want := runOn(t, e, "ZRANGE", "z", "0", "-1", "WITHSCORES")
	for _, parts := range [][]string{
		{"ZINCRBY", "z", "-inf", "a"}, {"ZINCRBY", "z", "nan", "b"},
		{"ZPOPMIN", "z", "-1"}, {"ZPOPMAX", "z", "x"},
		{"ZCOUNT", "z", "nan", "1"}, {"ZRANGEBYSCORE", "z", "0", "9", "LIMIT", "0"},
		{"ZRANGEBYSCORE", "z", "0", "9", "LIMIT", "bad", "1"},
	} {
		require.Equal(t, byte('-'), rawReplyOn(t, e, parts[0], parts[1:]...)[0], parts)
		require.Equal(t, want, runOn(t, e, "ZRANGE", "z", "0", "-1", "WITHSCORES"))
	}
	runOn(t, e, "SET", "wrong", "v")
	for _, parts := range [][]string{{"ZCOUNT", "wrong", "0", "1"}, {"ZRANGEBYSCORE", "wrong", "0", "1"}, {"ZINCRBY", "wrong", "1", "a"}, {"ZPOPMAX", "wrong"}} {
		require.Contains(t, string(rawReplyOn(t, e, parts[0], parts[1:]...)), "WRONGTYPE")
	}
}
