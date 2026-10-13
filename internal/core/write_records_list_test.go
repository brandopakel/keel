package core

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// This file sorts after every file of the package's code, as
// write_records_test.go does, so that its tests move no handler in the test
// binary the paired command-path job measures.

// listStateOn is each of keys' elements and expiry instant on e, read
// without reaping, for comparing a replica with its primary.
func listStateOn(e *Engine, keys ...string) map[string]string {
	state := map[string]string{}
	for _, key := range keys {
		l, ok := e.listStore.Peek(key)
		if !ok {
			state[key] = "(none)"
			continue
		}
		at, expires := e.listStore.GetExpiry(key)
		state[key] = strings.Join(l.All(), ",") + " expires " + strconv.FormatBool(expires) + " " + strconv.FormatUint(at, 10)
	}
	return state
}

// TestReplicaAppliesTheListCommandsRecords: a protocol 2 replica applies the
// records of every list command, each as the primary staged it - LPUSHX,
// RPUSHX, LINSERT, LREM, LMOVE, RPOPLPUSH and LMOVEM as themselves, LMPOP as
// the LPOP or RPOP with a count that replays it - and ends with the
// primary's lists and expiries. The writes that change nothing record
// nothing, and the replica ends the same without them.
func TestReplicaAppliesTheListCommandsRecords(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	setupReplicationV2On(t, e)
	frames := snapshotV2On(t, e)
	base, epoch := frames[0].To, frames[0].Epoch
	for _, cmd := range [][]string{
		{"RPUSH", "l", "a", "b", "c", "d"}, {"PEXPIREAT", "l", "4102444800123"},
		{"LPUSHX", "l", "z"}, {"RPUSHX", "l", "e", "f"}, {"LPUSHX", "none", "x"},
		{"LINSERT", "l", "BEFORE", "c", "X"}, {"LINSERT", "l", "after", "X", "Y"}, {"LINSERT", "l", "AFTER", "nope", "y"},
		{"LREM", "l", "1", "a"}, {"RPUSH", "r", "x", "y", "x", "z", "x"}, {"LREM", "r", "-2", "x"}, {"LREM", "r", "0", "nope"},
		{"LMOVE", "l", "dst", "LEFT", "RIGHT"}, {"RPOPLPUSH", "l", "dst"}, {"LMOVE", "dst", "dst", "LEFT", "RIGHT"},
		{"LMOVE", "none", "dst", "LEFT", "LEFT"},
		{"LMOVEM", "l", "m2", "RIGHT", "LEFT", "COUNT", "2", "BULK"}, {"LMOVEM", "l", "m2", "LEFT", "LEFT", "COUNT", "2", "OBO"},
		{"LMOVEM", "r", "r", "LEFT", "RIGHT", "EXACTLY", "2", "OBO"}, {"LMOVEM", "l", "m2", "LEFT", "LEFT", "EXACTLY", "9", "BULK"},
		{"LMOVEM", "dst", "m3", "RIGHT", "RIGHT"},
		{"LMPOP", "2", "none", "r", "RIGHT", "COUNT", "2"}, {"RPUSH", "gone", "only"}, {"LMPOP", "1", "gone", "LEFT", "COUNT", "9"},
		{"LPOP", "l", "0"}, {"LTRIM", "l", "0", "-1"}, {"LTRIM", "none", "0", "-1"}, {"LPOP", "none"},
	} {
		reply := rawReplyOn(t, e, cmd[0], cmd[1:]...)
		require.NotEqual(t, byte('-'), reply[0], "%v: %q", cmd, reply)
	}
	delta := pullV2On(t, e, epoch, base, "", 0)
	keys := []string{"l", "r", "dst", "m2", "m3", "none", "gone"}
	want := listStateOn(e, keys...)
	require.Equal(t, "Y,c expires true 4102444800123", want["l"])
	require.Equal(t, "z expires false 0", want["r"])
	require.Equal(t, "X,b,d,e expires false 0", want["m2"])
	require.Equal(t, "(none)", want["gone"])
	require.Equal(t, "(none)", want["none"])
	becomeReplicaV2On(t, e)
	for _, frame := range frames {
		require.NoError(t, e.ApplyReplication(frame))
	}
	require.NoError(t, e.ApplyReplication(delta))
	require.True(t, e.replicaReady)
	require.Equal(t, want, listStateOn(e, keys...))
}
