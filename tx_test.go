package keel_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brandopakel/keel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// records is the log's encoding of each of records, in turn.
func records(records ...[]string) string {
	var b strings.Builder
	for _, record := range records {
		fmt.Fprintf(&b, "*%d\r\n", len(record))
		for _, part := range record {
			fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(part), part)
		}
	}
	return b.String()
}

// TestAtomicIsOneFrameInTheLogAndReplays: what an Atomic writes is logged as
// one MULTI ... EXEC block, each command as itself, and a reopened instance
// replays it; a read in it answers what the commands before it wrote.
func TestAtomicIsOneFrameInTheLogAndReplays(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "log.aof")
	db, err := keel.Open(ctx, persistent(t, path, keel.FsyncAlways))
	require.NoError(t, err)
	at := time.UnixMilli(4102444800123)
	var set *keel.Result[struct{}]
	var read *keel.Result[string]
	var n *keel.Result[int64]
	var many *keel.Result[map[string]string]
	err = db.Atomic(ctx, func(tx *keel.Tx) error {
		set = tx.Set("k", "v", keel.WithExpireAt(at))
		read = tx.Get("k")
		n = tx.IncrBy("n", 5)
		tx.MSet(map[string]string{"y": "2", "x": "1"})
		many = tx.MGet("x", "y", "nobody")
		_, err := read.Value()
		assert.Error(t, err, "nothing has run while fn queues")
		return nil
	})
	require.NoError(t, err)
	require.NoError(t, set.Err())
	v, err := read.Value()
	require.NoError(t, err)
	assert.Equal(t, "v", v)
	count, err := n.Value()
	require.NoError(t, err)
	assert.EqualValues(t, 5, count)
	m, err := many.Value()
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"x": "1", "y": "2"}, m)

	// A transaction of reads logs nothing, not even its frame.
	require.NoError(t, db.Atomic(ctx, func(tx *keel.Tx) error {
		tx.Get("k")
		tx.Exists("k", "n")
		return nil
	}))
	require.NoError(t, db.Close())

	body, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, records([]string{"MULTI"}, []string{"SET", "k", "v"}, []string{"PEXPIREAT", "k", "4102444800123"},
		[]string{"INCRBY", "n", "5"}, []string{"MSET", "x", "1", "y", "2"}, []string{"EXEC"}), string(body))

	db = open(t, persistent(t, path, keel.FsyncAlways))
	when, err := db.ExpireTime(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, at, when)
	got, err := db.MGet(ctx, "k", "n", "x", "y")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"k": "v", "n": "5", "x": "1", "y": "2"}, got)
}

// TestAtomicFailingCommandUndoesNothing: a command that fails inside a
// transaction fails alone, as in Redis's EXEC - the commands before it are not
// undone and those after it run - and Atomic returns nil, the failure being
// its Result's. The failed command is not logged, and the rest replay.
func TestAtomicFailingCommandUndoesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "log.aof")
	db, err := keel.Open(ctx, persistent(t, path, keel.FsyncAlways))
	require.NoError(t, err)
	require.NoError(t, db.Set(ctx, "text", "not a number"))
	_, err = db.Do(ctx, "RPUSH", "list", "a")
	require.NoError(t, err)

	var first, last *keel.Result[struct{}]
	var incr *keel.Result[int64]
	var wrong *keel.Result[string]
	var badOption *keel.Result[bool]
	require.NoError(t, db.Atomic(ctx, func(tx *keel.Tx) error {
		first = tx.Set("a", "1")
		incr = tx.Incr("text")
		wrong = tx.Get("list")
		badOption = tx.SetNX("c", "3", keel.WithTTL(time.Microsecond))
		last = tx.Set("b", "2")
		return nil
	}))
	require.NoError(t, first.Err())
	assert.ErrorIs(t, incr.Err(), keel.ErrNotInteger)
	assert.EqualError(t, incr.Err(), "ERR value is not an integer or out of range")
	assert.ErrorIs(t, wrong.Err(), keel.ErrWrongType)
	assert.EqualError(t, badOption.Err(), "ERR invalid expire time in 'set' command")
	require.NoError(t, last.Err())
	for key, want := range map[string]string{"a": "1", "b": "2", "text": "not a number"} {
		v, err := db.Get(ctx, key)
		require.NoError(t, err)
		assert.Equal(t, want, v, key)
	}
	_, err = db.Get(ctx, "c")
	assert.ErrorIs(t, err, keel.ErrNotFound)
	require.NoError(t, db.Close())

	body, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(string(body),
		records([]string{"MULTI"}, []string{"SET", "a", "1"}, []string{"SET", "b", "2"}, []string{"EXEC"})), "%q", body)
	db = open(t, persistent(t, path, keel.FsyncAlways))
	for key, want := range map[string]string{"a": "1", "b": "2"} {
		v, err := db.Get(ctx, key)
		require.NoError(t, err)
		assert.Equal(t, want, v, key)
	}
}

// TestAtomicRunsWholeOrNotAtAll: a transaction whose fn fails, whose context
// is done, or whose DB is closed runs none of its commands, and their
// Results hold the reason; a command queued after Atomic returned never runs.
func TestAtomicRunsWholeOrNotAtAll(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := keel.Open(ctx, keel.Options{})
	require.NoError(t, err)

	mine := errors.New("changed my mind")
	var set *keel.Result[struct{}]
	var kept *keel.Tx
	err = db.Atomic(ctx, func(tx *keel.Tx) error {
		kept = tx
		set = tx.Set("k", "v")
		return mine
	})
	assert.Equal(t, mine, err)
	assert.Equal(t, mine, set.Err())
	_, err = db.Get(ctx, "k")
	assert.ErrorIs(t, err, keel.ErrNotFound, "nothing ran")
	late := kept.Set("late", "v")
	assert.Error(t, late.Err())
	_, err = db.Get(ctx, "late")
	assert.ErrorIs(t, err, keel.ErrNotFound)

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	err = db.Atomic(cancelled, func(tx *keel.Tx) error {
		set = tx.Set("k", "v")
		return nil
	})
	assert.ErrorIs(t, err, context.Canceled)
	assert.ErrorIs(t, set.Err(), context.Canceled)
	_, err = db.Get(ctx, "k")
	assert.ErrorIs(t, err, keel.ErrNotFound)

	stats, err := db.Stats(ctx)
	require.NoError(t, err)
	require.NoError(t, db.Atomic(ctx, func(tx *keel.Tx) error {
		tx.Set("k", "v")
		tx.Get("k")
		tx.MGet()
		return nil
	}))
	after, err := db.Stats(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 2+2, after.Commands-stats.Commands, "SET and GET, with MULTI and EXEC")

	require.NoError(t, db.Close())
	err = db.Atomic(ctx, func(tx *keel.Tx) error {
		set = tx.Set("k", "v")
		return nil
	})
	assert.ErrorIs(t, err, keel.ErrClosed)
	assert.ErrorIs(t, set.Err(), keel.ErrClosed)
}

// TestAtomicIsIsolated: no other call sees the instance between a
// transaction's commands.
func TestAtomicIsIsolated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := open(t, keel.Options{})
	require.NoError(t, db.Set(ctx, "k", "settled"))
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			v, err := db.Get(ctx, "k")
			assert.NoError(t, err)
			if v != "settled" {
				assert.Fail(t, "a reader saw inside a transaction", v)
				return
			}
		}
	}()
	for range 1000 {
		require.NoError(t, db.Atomic(ctx, func(tx *keel.Tx) error {
			tx.Set("k", "between")
			tx.Incr("count")
			tx.Set("k", "settled")
			return nil
		}))
	}
	close(stop)
	wg.Wait()
	n, err := db.Get(ctx, "count")
	require.NoError(t, err)
	assert.Equal(t, "1000", n)
}

// pack is a method's answer as a step of TestTxMethodsAnswerAsTheDBs takes it.
func pack[T any](v T, err error) (any, error) { return v, err }

// answer is a Result's answer, read once its transaction has run.
func answer[T any](r *keel.Result[T]) func() (any, error) {
	return func() (any, error) { return pack(r.Value()) }
}

// done is the answer of a method whose answer is its error alone.
func done(err error) (any, error) { return struct{}{}, err }

// TestTxMethodsAnswerAsTheDBs: each method of a Tx runs the DB's method of
// the same name, as the command it says it queues, and answers as it does,
// run in turn on an instance in the same state: the same value, the same
// error text, the same category.
func TestTxMethodsAnswerAsTheDBs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	viaTx, direct := open(t, keel.Options{}), open(t, keel.Options{})
	at := time.UnixMilli(4102444800123)

	type step struct {
		tx func(*keel.Tx) func() (any, error)
		db func(*keel.DB) (any, error)
	}
	steps := []step{
		{
			func(tx *keel.Tx) func() (any, error) { return answer(tx.Set("s", "hello")) },
			func(db *keel.DB) (any, error) { return done(db.Set(ctx, "s", "hello")) },
		}, {
			func(tx *keel.Tx) func() (any, error) { return answer(tx.SetNX("s", "x")) },
			func(db *keel.DB) (any, error) { return pack(db.SetNX(ctx, "s", "x")) },
		}, {
			func(tx *keel.Tx) func() (any, error) { return answer(tx.SetXX("s", "world", keel.WithKeepTTL())) },
			func(db *keel.DB) (any, error) { return pack(db.SetXX(ctx, "s", "world", keel.WithKeepTTL())) },
		}, {
			func(tx *keel.Tx) func() (any, error) { return answer(tx.Set("bad", "v", keel.WithTTL(0))) },
			func(db *keel.DB) (any, error) { return done(db.Set(ctx, "bad", "v", keel.WithTTL(0))) },
		}, {
			func(tx *keel.Tx) func() (any, error) { return answer(tx.SetBytes("b", []byte{0, 1})) },
			func(db *keel.DB) (any, error) { return done(db.SetBytes(ctx, "b", []byte{0, 1})) },
		}, {
			func(tx *keel.Tx) func() (any, error) { return answer(tx.GetBytes("b")) },
			func(db *keel.DB) (any, error) { return pack(db.GetBytes(ctx, "b")) },
		}, {
			func(tx *keel.Tx) func() (any, error) { return answer(tx.Append("s", "!")) },
			func(db *keel.DB) (any, error) { return pack(db.Append(ctx, "s", "!")) },
		}, {
			func(tx *keel.Tx) func() (any, error) { return answer(tx.StrLen("s")) },
			func(db *keel.DB) (any, error) { return pack(db.StrLen(ctx, "s")) },
		}, {
			func(tx *keel.Tx) func() (any, error) { return answer(tx.GetRange("s", 1, -2)) },
			func(db *keel.DB) (any, error) { return pack(db.GetRange(ctx, "s", 1, -2)) },
		}, {
			func(tx *keel.Tx) func() (any, error) { return answer(tx.SetRange("s", 7, "x")) },
			func(db *keel.DB) (any, error) { return pack(db.SetRange(ctx, "s", 7, "x")) },
		}, {
			func(tx *keel.Tx) func() (any, error) { return answer(tx.SetRange("s", -1, "x")) },
			func(db *keel.DB) (any, error) { return pack(db.SetRange(ctx, "s", -1, "x")) },
		}, {
			func(tx *keel.Tx) func() (any, error) { return answer(tx.Incr("n")) },
			func(db *keel.DB) (any, error) { return pack(db.Incr(ctx, "n")) },
		}, {
			func(tx *keel.Tx) func() (any, error) { return answer(tx.DecrBy("n", 10)) },
			func(db *keel.DB) (any, error) { return pack(db.DecrBy(ctx, "n", 10)) },
		}, {
			func(tx *keel.Tx) func() (any, error) { return answer(tx.Decr("n")) },
			func(db *keel.DB) (any, error) { return pack(db.Decr(ctx, "n")) },
		}, {
			func(tx *keel.Tx) func() (any, error) { return answer(tx.IncrBy("s", 1)) },
			func(db *keel.DB) (any, error) { return pack(db.IncrBy(ctx, "s", 1)) },
		}, {
			func(tx *keel.Tx) func() (any, error) { return answer(tx.IncrByFloat("f", 0.1)) },
			func(db *keel.DB) (any, error) { return pack(db.IncrByFloat(ctx, "f", 0.1)) },
		}, {
			func(tx *keel.Tx) func() (any, error) { return answer(tx.MSet(map[string]string{"m1": "1", "m2": "2"})) },
			func(db *keel.DB) (any, error) { return done(db.MSet(ctx, map[string]string{"m1": "1", "m2": "2"})) },
		}, {
			func(tx *keel.Tx) func() (any, error) { return answer(tx.MGet("m1", "m2", "none", "s")) },
			func(db *keel.DB) (any, error) { return pack(db.MGet(ctx, "m1", "m2", "none", "s")) },
		}, {
			func(tx *keel.Tx) func() (any, error) { return answer(tx.MGet()) },
			func(db *keel.DB) (any, error) { return pack(db.MGet(ctx)) },
		}, {
			func(tx *keel.Tx) func() (any, error) { return answer(tx.CompareAndSwap("m1", "1", "one")) },
			func(db *keel.DB) (any, error) { return pack(db.CompareAndSwap(ctx, "m1", "1", "one")) },
		}, {
			func(tx *keel.Tx) func() (any, error) { return answer(tx.DeleteIf("m2", "nope")) },
			func(db *keel.DB) (any, error) { return pack(db.DeleteIf(ctx, "m2", "nope")) },
		}, {
			func(tx *keel.Tx) func() (any, error) { return answer(tx.ExpireAt("s", at)) },
			func(db *keel.DB) (any, error) { return pack(db.ExpireAt(ctx, "s", at)) },
		}, {
			func(tx *keel.Tx) func() (any, error) { return answer(tx.ExpireTime("s")) },
			func(db *keel.DB) (any, error) { return pack(db.ExpireTime(ctx, "s")) },
		}, {
			func(tx *keel.Tx) func() (any, error) { return answer(tx.Expire("s", time.Hour, keel.ExpireGT)) },
			func(db *keel.DB) (any, error) { return pack(db.Expire(ctx, "s", time.Hour, keel.ExpireGT)) },
		}, {
			func(tx *keel.Tx) func() (any, error) { return answer(tx.Persist("s")) },
			func(db *keel.DB) (any, error) { return pack(db.Persist(ctx, "s")) },
		}, {
			func(tx *keel.Tx) func() (any, error) { return answer(tx.TTL("s")) },
			func(db *keel.DB) (any, error) { return pack(db.TTL(ctx, "s")) },
		}, {
			func(tx *keel.Tx) func() (any, error) { return answer(tx.TTL("none")) },
			func(db *keel.DB) (any, error) { return pack(db.TTL(ctx, "none")) },
		}, {
			func(tx *keel.Tx) func() (any, error) { return answer(tx.Type("s")) },
			func(db *keel.DB) (any, error) { return pack(db.Type(ctx, "s")) },
		}, {
			func(tx *keel.Tx) func() (any, error) { return answer(tx.Exists("s", "s", "none")) },
			func(db *keel.DB) (any, error) { return pack(db.Exists(ctx, "s", "s", "none")) },
		}, {
			func(tx *keel.Tx) func() (any, error) { return answer(tx.RenameNX("s", "m1")) },
			func(db *keel.DB) (any, error) { return pack(db.RenameNX(ctx, "s", "m1")) },
		}, {
			func(tx *keel.Tx) func() (any, error) { return answer(tx.Rename("s", "moved")) },
			func(db *keel.DB) (any, error) { return done(db.Rename(ctx, "s", "moved")) },
		}, {
			func(tx *keel.Tx) func() (any, error) { return answer(tx.Rename("none", "x")) },
			func(db *keel.DB) (any, error) { return done(db.Rename(ctx, "none", "x")) },
		}, {
			func(tx *keel.Tx) func() (any, error) { return answer(tx.GetDel("moved")) },
			func(db *keel.DB) (any, error) { return pack(db.GetDel(ctx, "moved")) },
		}, {
			func(tx *keel.Tx) func() (any, error) { return answer(tx.Get("moved")) },
			func(db *keel.DB) (any, error) { return pack(db.Get(ctx, "moved")) },
		}, {
			func(tx *keel.Tx) func() (any, error) { return answer(tx.Del("m1", "m2", "none")) },
			func(db *keel.DB) (any, error) { return pack(db.Del(ctx, "m1", "m2", "none")) },
		}, {
			func(tx *keel.Tx) func() (any, error) { return answer(tx.DBSize()) },
			func(db *keel.DB) (any, error) { return pack(db.DBSize(ctx)) },
		}, {
			func(tx *keel.Tx) func() (any, error) { return answer(tx.FlushDB()) },
			func(db *keel.DB) (any, error) { return done(db.FlushDB(ctx)) },
		}, {
			func(tx *keel.Tx) func() (any, error) { return answer(tx.DBSize()) },
			func(db *keel.DB) (any, error) { return pack(db.DBSize(ctx)) },
		},
	}
	var answers []func() (any, error)
	require.NoError(t, viaTx.Atomic(ctx, func(tx *keel.Tx) error {
		for _, s := range steps {
			answers = append(answers, s.tx(tx))
		}
		return nil
	}))
	for i, s := range steps {
		want, wantErr := s.db(direct)
		got, gotErr := answers[i]()
		if d, ok := want.(time.Duration); ok {
			// Measured a moment apart.
			assert.InDelta(t, float64(d), float64(got.(time.Duration)), float64(time.Second), "step %d", i)
		} else {
			assert.Equal(t, want, got, "step %d", i)
		}
		if wantErr == nil {
			assert.NoError(t, gotErr, "step %d", i)
			continue
		}
		require.Error(t, gotErr, "step %d", i)
		assert.Equal(t, wantErr.Error(), gotErr.Error(), "step %d", i)
		for _, category := range []error{keel.ErrNotFound, keel.ErrWrongType, keel.ErrNotInteger, keel.ErrOverflow} {
			assert.Equal(t, errors.Is(wantErr, category), errors.Is(gotErr, category), "step %d: %v", i, category)
		}
	}
}
