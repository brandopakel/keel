package core

import (
	"testing"

	"github.com/brandopakel/keel/internal/data_structure"
	"github.com/stretchr/testify/require"
)

// This file sorts after every file of the package's code, so that its tests,
// compiled into package core, move none of the handlers in the test binary
// the paired command-path job measures (docs/phase4-family-recipe.md,
// step 5).

// TestZAddOfAGoCallerIsRecordedAsZAdd: a ZADD with no client text, as a Go
// caller makes it, is recorded as the ZADD of the members it changed at the
// scores they now have, and nothing when it changed none; a GEOADD as the
// ZADD of its geohash scores, the form Redis propagates GEOADD in (geo.c,
// lines 490-503); and a client's ZADD as the text it sent.
func TestZAddOfAGoCallerIsRecordedAsZAdd(t *testing.T) {
	t.Parallel()
	palermo, err := geoaddScore("13.361389", "38.115556")
	require.NoError(t, err)

	e := stagingEngine(t)
	e.aofBegin("ZADD")
	res, err := e.opZAdd("z", zaddRequest{flags: data_structure.ZAddNX,
		scores: []float64{1.5, 2}, members: []string{"a", "b"}})
	require.NoError(t, err)
	require.Equal(t, [2]int64{2, 2}, [2]int64{res.added, res.changed})
	// A Go caller's ZADD that changes nothing records nothing, and one that
	// changes some members records those at the scores they now have.
	_, err = e.opZAdd("z", zaddRequest{flags: data_structure.ZAddNX, scores: []float64{9}, members: []string{"a"}})
	require.NoError(t, err)
	_, err = e.opZAdd("z", zaddRequest{flags: data_structure.ZAddIncr, scores: []float64{0.5}, members: []string{"a"}})
	require.NoError(t, err)
	res, err = e.opGeoAdd("g", zaddRequest{scores: []float64{palermo}, members: []string{"palermo"},
		text: []string{"g", "13.361389", "38.115556", "palermo"}})
	require.NoError(t, err)
	require.EqualValues(t, 1, res.added)
	_, err = e.opZAdd("z", zaddRequest{scores: []float64{3}, members: []string{"c"},
		text: []string{"z", "ch", "3", "c"}})
	require.NoError(t, err)
	e.appendStaged()
	require.NoError(t, e.FlushAOF())

	want := stagingEngine(t)
	want.aofBegin("ZADD")
	want.appendAOFCommand("ZADD", "z", "1.5", "a", "2", "b")
	want.appendAOFCommand("ZADD", "z", "2", "a")
	want.appendAOFCommand("ZADD", "g", "3479099956230698", "palermo")
	want.appendAOFCommand("ZADD", "z", "ch", "3", "c")
	require.NoError(t, want.FlushAOF())
	require.Equal(t, readLog(t, want), readLog(t, e))

	// The ZADD of the scores puts the member where GEOADD would.
	client := newTestEngine(t, Options{})
	runOn(t, client, "GEOADD", "g", "13.361389", "38.115556", "palermo")
	replayed := newTestEngine(t, Options{})
	runOn(t, replayed, "ZADD", "g", "3479099956230698", "palermo")
	require.Equal(t, runOn(t, client, "GEOPOS", "g", "palermo"), runOn(t, replayed, "GEOPOS", "g", "palermo"))
}

// TestReplicaAppliesAZPopThatPopsNothing: a ZPOPMIN or ZPOPMAX that pops
// nothing - a missing key, with and without a count, or a count of 0 -
// records nothing, and one that pops records the ZREM of what it popped,
// which a protocol 2 replica, whose stream is the log, applies.
func TestReplicaAppliesAZPopThatPopsNothing(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	setupReplicationV2On(t, e)
	runOn(t, e, "ZADD", "z", "1", "a", "2", "b", "3", "c", "4", "d")
	frames := snapshotV2On(t, e)
	base, epoch := frames[0].To, frames[0].Epoch
	for _, cmd := range [][]string{
		{"ZPOPMIN", "missing"}, {"ZPOPMAX", "missing", "2"}, {"ZPOPMIN", "z", "0"},
		{"ZPOPMIN", "z"}, {"ZPOPMAX", "z", "2"},
	} {
		reply := rawReplyOn(t, e, cmd[0], cmd[1:]...)
		require.NotEqual(t, byte('-'), reply[0], "%v: %q", cmd, reply)
	}
	delta := pullV2On(t, e, epoch, base, "", 0)
	require.NotContains(t, string(delta.Body), "ZPOP", "a ZPOPMIN or ZPOPMAX is recorded as the ZREM of what it popped, or not at all")
	want := runOn(t, e, "ZRANGE", "z", "0", "-1", "WITHSCORES")
	require.Equal(t, []interface{}{"b", "2"}, want)
	becomeReplicaV2On(t, e)
	for _, frame := range frames {
		require.NoError(t, e.ApplyReplication(frame))
	}
	require.NoError(t, e.ApplyReplication(delta))
	require.True(t, e.replicaReady)
	require.Equal(t, want, runOn(t, e, "ZRANGE", "z", "0", "-1", "WITHSCORES"))
}
