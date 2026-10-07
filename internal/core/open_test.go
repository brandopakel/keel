package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// logRecord is one command as the log records it.
func logRecord(parts ...string) string { return string(appendCommand(nil, parts...)) }

// openTestEngine opens an engine for t with Open, and closes it when t ends,
// after t's temporary directory is taken, as newTestEngine does.
func openTestEngine(t *testing.T, o Options) *Engine {
	t.Helper()
	t.TempDir()
	e, err := Open(context.Background(), o)
	require.NoError(t, err)
	t.Cleanup(func() { _ = e.Close() })
	return e
}

// filesIn reads every file in dir, by name.
func filesIn(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	files := map[string]string{}
	for _, entry := range entries {
		body, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		require.NoError(t, err)
		files[entry.Name()] = string(body)
	}
	return files
}

// countingContext reports itself cancelled from the cancelAt-th time its Err
// is asked for, so a test can stop a replay at a point it chooses by count
// rather than by timing. Only Err is counted: the replay asks nothing else.
type countingContext struct {
	context.Context
	cancelAt int64
	calls    atomic.Int64
}

func (c *countingContext) Err() error {
	if c.calls.Add(1) >= c.cancelAt {
		return context.Canceled
	}
	return nil
}

func cancelOnLook(n int64) *countingContext {
	return &countingContext{Context: context.Background(), cancelAt: n}
}

// TestOpenReplaysTheLogCloseLeft: an engine Open makes replays its log and
// opens it, and Close writes and syncs what is buffered, whatever the fsync
// policy, so the next Open gives back every key of every type.
func TestOpenReplaysTheLogCloseLeft(t *testing.T) {
	t.Parallel()
	for _, policy := range []FsyncPolicy{FsyncAlways, FsyncEverySec, FsyncNever} {
		t.Run(string(policy), func(t *testing.T) {
			t.Parallel()
			o := Options{AppendOnly: true, AppendFilename: filepath.Join(t.TempDir(), "keel.aof"), Fsync: policy}
			e := openTestEngine(t, o)
			require.True(t, e.AOFEnabled())
			fillOneOfEverythingOn(t, e)
			want := snapshotEverythingOn(t, e)
			require.NoError(t, e.Close(), "nothing was flushed before Close: it has to write it all")
			require.ErrorIs(t, e.Close(), ErrClosed, "closing twice is closing a closed engine")

			again := openTestEngine(t, o)
			assert.Equal(t, want, snapshotEverythingOn(t, again))
		})
	}
}

// TestOpenWithoutALog: an engine with no log opens empty, and closes.
func TestOpenWithoutALog(t *testing.T) {
	t.Parallel()
	e := openTestEngine(t, Options{})
	assert.False(t, e.AOFEnabled())
	assert.Equal(t, int64(0), runOn(t, e, "DBSIZE"))
	assert.Equal(t, "OK", runOn(t, e, "SET", "k", "v"))
	require.NoError(t, e.Close())
	require.ErrorIs(t, e.Close(), ErrClosed)
}

// TestOpenRepairsATornTail: a log whose last command was cut short by a crash
// opens on the commands before it; the tail is kept beside the log, and the
// log is truncated where it began.
func TestOpenRepairsATornTail(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "keel.aof")
	intact := logRecord("SET", "a", "1") + logRecord("SET", "b", "2")
	torn := logRecord("SET", "c", "3")[:11]
	require.NoError(t, os.WriteFile(path, []byte(intact+torn), 0o644))

	e := openTestEngine(t, Options{AppendOnly: true, AppendFilename: path})
	assert.Equal(t, "1", runOn(t, e, "GET", "a"))
	assert.Equal(t, "2", runOn(t, e, "GET", "b"))
	assert.Equal(t, "$-1\r\n", string(rawReplyOn(t, e, "GET", "c")), "the torn command is not replayed")
	require.NoError(t, e.Close())

	files := filesIn(t, dir)
	assert.Equal(t, intact, files["keel.aof"], "the log ends where the torn command began")
	var backups []string
	for name, body := range files {
		if strings.HasPrefix(name, ".keel-torn-tail-") {
			backups = append(backups, body)
		}
	}
	assert.Equal(t, []string{torn}, backups, "the torn tail is kept, exactly")
}

// TestOpenRefusesADamagedLogAndLeavesItAlone: damage before the end of a log
// is a refusal to start, which writes nothing: no repair, and no log opened.
func TestOpenRefusesADamagedLogAndLeavesItAlone(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "keel.aof")
	require.NoError(t, os.WriteFile(path, []byte(logRecord("SET", "a", "1")+"garbage\r\n"+logRecord("SET", "b", "2")), 0o644))
	before := filesIn(t, dir)

	e, err := Open(context.Background(), Options{AppendOnly: true, AppendFilename: path})
	require.Error(t, err)
	assert.Nil(t, e)
	assert.True(t, strings.HasPrefix(err.Error(), "appendonly: malformed command at byte "), "%v", err)
	assert.Equal(t, before, filesIn(t, dir))
}

// TestOpenRefusesOptionsNoEngineCanBeHeldTo, before it touches anything.
func TestOpenRefusesOptionsNoEngineCanBeHeldTo(t *testing.T) {
	t.Parallel()
	e, err := Open(context.Background(), Options{AppendOnly: true})
	require.EqualError(t, err, "AppendOnly needs an AppendFilename")
	assert.Nil(t, e)
}

// TestACancelledReplayLeavesEveryFileAsItWas: a startup stopped by its
// context, before or during the replay, or once the replay has finished but
// before anything is written, returns the context's error and leaves every
// file the same bytes - here including a torn tail it must not repair, and a
// term file - with no log created where there was none. A later Open replays
// all of it.
func TestACancelledReplayLeavesEveryFileAsItWas(t *testing.T) {
	t.Parallel()
	const keys = 5000
	torn := logRecord("SET", "late", "v")[:9]

	cases := []struct {
		name string
		// look is when the context reports itself cancelled: the first look
		// is Open's own, the next ones the replay's, every replayCheckEvery
		// records from the first, and the last the one after the replay.
		look int64
		// records is how many records the log holds before its torn tail.
		records int
		wantErr string
	}{
		{name: "before Open starts", look: 1, records: keys, wantErr: "context canceled"},
		{name: "before the first record", look: 2, records: keys, wantErr: "appendonly: replay of %s stopped at byte 0: context canceled"},
		{name: "part of the way through", look: 4, records: keys, wantErr: "appendonly: replay of %s stopped at byte %d: context canceled"},
		{name: "after the replay, before the repair", look: 3, records: 10, wantErr: "appendonly: startup stopped after replaying %s: context canceled"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "keel.aof")
			var body strings.Builder
			// lookAt is where each look at the context falls, in bytes.
			lookAt := map[int]int{}
			for i := range c.records {
				if i%replayCheckEvery == 0 {
					lookAt[i/replayCheckEvery] = body.Len()
				}
				body.WriteString(logRecord("SET", fmt.Sprintf("k%d", i), "v"))
			}
			require.NoError(t, os.WriteFile(path, []byte(body.String()+torn), 0o644))
			require.NoError(t, os.WriteFile(path+termFileName, []byte("0"), 0o644))
			before := filesIn(t, dir)

			ctx := cancelOnLook(c.look)
			e, err := Open(ctx, Options{AppendOnly: true, AppendFilename: path})
			require.ErrorIs(t, err, context.Canceled)
			assert.Nil(t, e)
			want := c.wantErr
			if strings.Contains(want, "%d") {
				// The replay's third look, at record 2*replayCheckEvery.
				want = fmt.Sprintf(want, path, lookAt[2])
			} else if strings.Contains(want, "%s") {
				want = fmt.Sprintf(want, path)
			}
			assert.EqualError(t, err, want)
			assert.Equal(t, c.look, ctx.calls.Load(), "startup goes on no further once the context is done")
			assert.Equal(t, before, filesIn(t, dir), "nothing is written before the replay has finished")

			e = openTestEngine(t, Options{AppendOnly: true, AppendFilename: path})
			assert.Equal(t, int64(c.records), runOn(t, e, "DBSIZE"), "a later Open replays it all")
		})
	}

	// With no log yet, a startup stopped before it began creates none.
	t.Run("no log yet", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		e, err := Open(cancelOnLook(2), Options{AppendOnly: true, AppendFilename: filepath.Join(dir, "keel.aof")})
		require.ErrorIs(t, err, context.Canceled)
		assert.Nil(t, e)
		assert.Empty(t, filesIn(t, dir))
	})
}

// TestAReplayLooksAtItsContextOnlyOutsideABlock: a transaction's block is
// replayed whole or not at all, so a replay never looks at its context inside
// one. Here a block spans the look due at record replayCheckEvery, which is
// skipped, so the replay goes on to its next look, replayCheckEvery records
// later, and stops there.
func TestAReplayLooksAtItsContextOnlyOutsideABlock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "keel.aof")
	var records []string
	for i := range replayCheckEvery - 1 {
		records = append(records, logRecord("SET", fmt.Sprintf("k%d", i), "v"))
	}
	records = append(records, logRecord("MULTI"), logRecord("SET", "x", "1"), logRecord("SET", "y", "1"),
		logRecord("SET", "z", "1"), logRecord("EXEC"))
	for i := range 2 * replayCheckEvery {
		records = append(records, logRecord("SET", fmt.Sprintf("after%d", i), "v"))
	}
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(records, "")), 0o644))

	// The looks: Open's own, the replay's at record 0, and its next.
	ctx := cancelOnLook(3)
	e, err := Open(ctx, Options{AppendOnly: true, AppendFilename: path})
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, e)
	stoppedAt := len(strings.Join(records[:2*replayCheckEvery], ""))
	assert.EqualError(t, err, fmt.Sprintf("appendonly: replay of %s stopped at byte %d: context canceled", path, stoppedAt))
}

// TestStartAOFDoesNothingWithoutAppendOnly: the log's startup reads the
// engine's options, and with no log there is nothing to start.
func TestStartAOFDoesNothingWithoutAppendOnly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	e := newTestEngine(t, Options{AppendFilename: filepath.Join(dir, "keel.aof")})
	require.NoError(t, e.StartAOF(context.Background(), ""))
	assert.False(t, e.AOFEnabled())
	assert.Empty(t, filesIn(t, dir))
}

// The default log was ./memkv-master.aof before the rename and is
// ./keel-master.aof now. A restart that looked only at the new name would
// replay nothing and serve an empty keyspace beside the old log, silently.
func TestAOFReadPath(t *testing.T) {
	t.Parallel()
	write := func(t *testing.T, path string) {
		t.Helper()
		assert.NoError(t, os.WriteFile(path, []byte("*1\r\n$4\r\nPING\r\n"), 0o644))
	}

	t.Run("reads the current name when it is there", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		current := filepath.Join(dir, "keel-master.aof")
		legacy := filepath.Join(dir, "memkv-master.aof")
		write(t, current)
		assert.Equal(t, current, aofReadPath(current, legacy))
	})

	t.Run("falls back to the name used before the rename", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		current := filepath.Join(dir, "keel-master.aof")
		legacy := filepath.Join(dir, "memkv-master.aof")
		write(t, legacy)
		assert.Equal(t, legacy, aofReadPath(current, legacy),
			"a log written before the rename must still be found")
	})

	t.Run("prefers the current name when both exist", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		current := filepath.Join(dir, "keel-master.aof")
		legacy := filepath.Join(dir, "memkv-master.aof")
		write(t, current)
		write(t, legacy)
		assert.Equal(t, current, aofReadPath(current, legacy),
			"the old name is a fallback, not a merge")
	})

	t.Run("reports the current name when neither exists", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		current := filepath.Join(dir, "keel-master.aof")
		legacy := filepath.Join(dir, "memkv-master.aof")
		assert.Equal(t, current, aofReadPath(current, legacy),
			"a first start writes to the current name")
	})

	t.Run("reads the current name when there is no legacy name", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		current := filepath.Join(dir, "keel-master.aof")
		assert.Equal(t, current, aofReadPath(current, ""), "Open passes none")
	})
}
