package keel_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brandopakel/keel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func open(t *testing.T, o keel.Options) *keel.DB {
	t.Helper()
	db, err := keel.Open(context.Background(), o)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return db
}

func persistent(t *testing.T, path string, fsync keel.Fsync) keel.Options {
	return keel.Options{Persistence: &keel.Persistence{Path: path, Fsync: fsync}}
}

// TestTwoInstancesAreIsolated: two instances in one process share nothing -
// keys, logs or counts - in memory or on disk, and each reopens to its own.
func TestTwoInstancesAreIsolated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a, b := open(t, keel.Options{}), open(t, keel.Options{})
	require.NoError(t, a.Set(ctx, "k", "a"))
	_, err := b.Get(ctx, "k")
	assert.ErrorIs(t, err, keel.ErrNotFound)
	n, _ := b.DBSize(ctx)
	assert.Zero(t, n)

	dir := t.TempDir()
	pa, pb := filepath.Join(dir, "a.aof"), filepath.Join(dir, "b.aof")
	da, err := keel.Open(ctx, persistent(t, pa, keel.FsyncAlways))
	require.NoError(t, err)
	db, err := keel.Open(ctx, persistent(t, pb, keel.FsyncAlways))
	require.NoError(t, err)
	require.NoError(t, da.Set(ctx, "k", "a"))
	require.NoError(t, db.Set(ctx, "k", "b"))
	require.NoError(t, da.Close())
	require.NoError(t, db.Close())
	for path, want := range map[string]string{pa: "a", pb: "b"} {
		d := open(t, persistent(t, path, keel.FsyncEverySec))
		v, err := d.Get(ctx, "k")
		require.NoError(t, err)
		assert.Equal(t, want, v, path)
	}
}

// TestCallsContendSafely: many goroutines calling one instance at once, run
// under -race in CI, lose no increment and see no torn value.
func TestCallsContendSafely(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, o := range []keel.Options{{}, persistent(t, filepath.Join(t.TempDir(), "log.aof"), keel.FsyncEverySec)} {
		db := open(t, o)
		const workers, each = 16, 300
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				key := "k" + strconv.Itoa(w)
				for i := 0; i < each; i++ {
					_, err := db.Incr(ctx, "counter")
					assert.NoError(t, err)
					assert.NoError(t, db.Set(ctx, key, strconv.Itoa(i)))
					v, err := db.Get(ctx, key)
					assert.NoError(t, err)
					assert.Equal(t, strconv.Itoa(i), v)
				}
			}(w)
		}
		wg.Wait()
		v, err := db.Get(ctx, "counter")
		require.NoError(t, err)
		assert.Equal(t, strconv.Itoa(workers*each), v)
	}
}

const (
	crashChildEnv   = "KEEL_TEST_CRASH_CHILD"
	crashChildFsync = "KEEL_TEST_CRASH_FSYNC"
)

// crashChild writes key i = value i to the instance at path, for ever,
// printing each i once its Set has returned, until it is killed.
func crashChild(path, fsync string) {
	policy := map[string]keel.Fsync{"always": keel.FsyncAlways, "everysec": keel.FsyncEverySec, "no": keel.FsyncNo}[fsync]
	ctx := context.Background()
	db, err := keel.Open(ctx, keel.Options{Persistence: &keel.Persistence{Path: path, Fsync: policy}})
	if err != nil {
		fmt.Println("open:", err)
		os.Exit(2)
	}
	out := bufio.NewWriter(os.Stdout)
	for i := 0; ; i++ {
		if err := db.Set(ctx, "k"+strconv.Itoa(i), "v"+strconv.Itoa(i), keel.WithTTL(time.Hour)); err != nil {
			fmt.Println("set:", err)
			os.Exit(2)
		}
		fmt.Fprintln(out, i)
		out.Flush()
	}
}

// TestCrashAndReopen: a process killed while it writes loses no write a Set
// had returned from, under each fsync policy - a call returns once its record
// is written to the operating system, and synced under FsyncAlways - and the
// log the kill cut off reopens, repaired to its last whole record.
func TestCrashAndReopen(t *testing.T) {
	t.Parallel()
	for _, fsync := range []string{"always", "everysec", "no"} {
		t.Run(fsync, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "crash.aof")
			child := exec.Command(os.Args[0], "-test.run=^$")
			child.Env = append(os.Environ(), crashChildEnv+"="+path, crashChildFsync+"="+fsync)
			stdout, err := child.StdoutPipe()
			require.NoError(t, err)
			require.NoError(t, child.Start())
			lines := bufio.NewScanner(stdout)
			acked := -1
			for lines.Scan() && acked < 300 {
				i, err := strconv.Atoi(lines.Text())
				require.NoError(t, err, lines.Text())
				acked = i
			}
			require.NoError(t, child.Process.Kill())
			_ = child.Wait()
			require.GreaterOrEqual(t, acked, 300, "the child acknowledged writes before it was killed")

			ctx := context.Background()
			db := open(t, persistent(t, path, keel.FsyncEverySec))
			for i := 0; i <= acked; i++ {
				v, err := db.Get(ctx, "k"+strconv.Itoa(i))
				require.NoError(t, err, "acknowledged key %d", i)
				require.Equal(t, "v"+strconv.Itoa(i), v)
			}
			ttl, err := db.TTL(ctx, "k0")
			require.NoError(t, err)
			assert.Greater(t, ttl, 50*time.Minute, "the expiry replays as the instant it was set for")
		})
	}
}

// TestCancelledContext: a call whose context is done before it runs returns
// the context's error and changes nothing.
func TestCancelledContext(t *testing.T) {
	t.Parallel()
	db := open(t, keel.Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.ErrorIs(t, db.Set(ctx, "k", "v"), context.Canceled)
	_, err := db.Get(context.Background(), "k")
	assert.ErrorIs(t, err, keel.ErrNotFound, "the cancelled Set did not run")
	_, err = keel.Open(ctx, keel.Options{})
	assert.ErrorIs(t, err, context.Canceled)
}

// TestSecondOpenOfALogIsLocked: one instance per log: a second Open of the
// same path fails with ErrLocked until the first closes.
func TestSecondOpenOfALogIsLocked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "locked.aof")
	first, err := keel.Open(ctx, persistent(t, path, keel.FsyncEverySec))
	require.NoError(t, err)
	_, err = keel.Open(ctx, persistent(t, path, keel.FsyncEverySec))
	require.ErrorIs(t, err, keel.ErrLocked)
	assert.Contains(t, err.Error(), "log in use by another instance")
	require.NoError(t, first.Close())
	second, err := keel.Open(ctx, persistent(t, path, keel.FsyncEverySec))
	require.NoError(t, err)
	require.NoError(t, second.Close())
}

// TestErrorsKeepRedisText: an error answers errors.Is for its category and
// keeps the words Redis answers with.
func TestErrorsKeepRedisText(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := keel.Open(ctx, keel.Options{})
	require.NoError(t, err)

	require.NoError(t, db.Set(ctx, "s", "abc"))
	_, err = db.Incr(ctx, "s")
	assert.ErrorIs(t, err, keel.ErrNotInteger)
	assert.EqualError(t, err, "ERR value is not an integer or out of range")

	require.NoError(t, db.Set(ctx, "max", strconv.FormatInt(math.MaxInt64, 10)))
	_, err = db.Incr(ctx, "max")
	assert.ErrorIs(t, err, keel.ErrOverflow)
	assert.EqualError(t, err, "ERR increment or decrement would overflow")
	_, err = db.DecrBy(ctx, "s", math.MinInt64)
	assert.EqualError(t, err, "ERR decrement would overflow")
	assert.ErrorIs(t, err, keel.ErrOverflow)

	_, err = db.Do(ctx, "RPUSH", "list", "a")
	require.NoError(t, err)
	_, err = db.Get(ctx, "list")
	assert.ErrorIs(t, err, keel.ErrWrongType)
	assert.EqualError(t, err, "WRONGTYPE Operation against a key holding the wrong kind of value")
	_, err = db.Do(ctx, "INCR", "list")
	assert.ErrorIs(t, err, keel.ErrWrongType, "Do's errors have their categories too")

	assert.EqualError(t, db.Set(ctx, "k", "v", keel.WithTTL(0)), "ERR invalid expire time in 'set' command")
	assert.EqualError(t, db.Set(ctx, "k", "v", keel.WithTTL(time.Minute), keel.WithKeepTTL()), "ERR syntax error")
	_, err = db.Expire(ctx, "s", time.Minute, keel.ExpireNX, keel.ExpireGT)
	assert.EqualError(t, err, "ERR NX and XX, GT or LT options at the same time are not compatible")
	assert.EqualError(t, db.Rename(ctx, "nobody", "x"), "ERR no such key")
	_, err = db.Do(ctx, "NOSUCH", "a")
	assert.EqualError(t, err, "ERR unknown command 'NOSUCH', with args beginning with: 'a' ")

	require.NoError(t, db.Close())
	_, err = db.Get(ctx, "s")
	assert.ErrorIs(t, err, keel.ErrClosed)
	assert.ErrorIs(t, db.Close(), keel.ErrClosed, "a second Close")
}

// TestMethodsAreLoggedAsTheirCommands: what a method writes is logged as the
// command it is named after would be - SET with its expiry as PEXPIREAT,
// INCRBY as itself, a DEL that deleted nothing not at all - and replays.
func TestMethodsAreLoggedAsTheirCommands(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "log.aof")
	db, err := keel.Open(ctx, persistent(t, path, keel.FsyncAlways))
	require.NoError(t, err)
	at := time.UnixMilli(4102444800123)
	require.NoError(t, db.Set(ctx, "k", "v", keel.WithExpireAt(at)))
	_, err = db.IncrBy(ctx, "n", 5)
	require.NoError(t, err)
	_, err = db.Del(ctx, "nobody")
	require.NoError(t, err)
	_, err = db.Append(ctx, "k", "w")
	require.NoError(t, err)
	_, err = db.Persist(ctx, "k")
	require.NoError(t, err)
	_, err = db.Get(ctx, "k")
	require.NoError(t, err)
	require.NoError(t, db.Close())

	body, err := os.ReadFile(path)
	require.NoError(t, err)
	var want strings.Builder
	for _, record := range [][]string{
		{"SET", "k", "v"}, {"PEXPIREAT", "k", "4102444800123"}, {"INCRBY", "n", "5"}, {"APPEND", "k", "w"}, {"PERSIST", "k"},
	} {
		fmt.Fprintf(&want, "*%d\r\n", len(record))
		for _, part := range record {
			fmt.Fprintf(&want, "$%d\r\n%s\r\n", len(part), part)
		}
	}
	assert.Contains(t, string(body), want.String())

	db = open(t, persistent(t, path, keel.FsyncAlways))
	v, err := db.Get(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, "vw", v)
	ttl, err := db.TTL(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, keel.NoTTL, ttl)
	n, err := db.Get(ctx, "n")
	require.NoError(t, err)
	assert.Equal(t, "5", n)
}

// TestTypedMethodsAnswerAsTheirCommands checks the methods' answers at their
// edges against the commands'.
func TestTypedMethodsAnswerAsTheirCommands(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := open(t, keel.Options{})

	ok, err := db.SetNX(ctx, "k", "1")
	require.NoError(t, err)
	assert.True(t, ok)
	ok, _ = db.SetNX(ctx, "k", "2")
	assert.False(t, ok)
	ok, _ = db.SetXX(ctx, "missing", "x")
	assert.False(t, ok)
	ok, _ = db.SetXX(ctx, "k", "3", keel.WithTTL(time.Hour))
	assert.True(t, ok)
	require.NoError(t, db.Set(ctx, "k", "4", keel.WithKeepTTL()))
	ttl, _ := db.TTL(ctx, "k")
	assert.Greater(t, ttl, 59*time.Minute, "KEEPTTL kept it")
	require.NoError(t, db.Set(ctx, "k", "5"))
	ttl, _ = db.TTL(ctx, "k")
	assert.Equal(t, keel.NoTTL, ttl, "a plain SET clears it")

	f, err := db.IncrByFloat(ctx, "f", 10.5)
	require.NoError(t, err)
	assert.Equal(t, 10.5, f)
	f, _ = db.IncrByFloat(ctx, "f", 0.1)
	assert.Equal(t, 10.6, f, "in a long double's precision, as Redis adds")

	n, _ := db.SetRange(ctx, "r", 3, "x")
	assert.EqualValues(t, 4, n)
	r, _ := db.Get(ctx, "r")
	assert.Equal(t, "\x00\x00\x00x", r)
	s, _ := db.GetRange(ctx, "r", -1, -1)
	assert.Equal(t, "x", s)
	n, _ = db.StrLen(ctx, "nobody")
	assert.Zero(t, n)

	v, err := db.GetDel(ctx, "r")
	require.NoError(t, err)
	assert.Equal(t, "\x00\x00\x00x", v)
	_, err = db.GetDel(ctx, "r")
	assert.ErrorIs(t, err, keel.ErrNotFound)

	n, _ = db.Exists(ctx, "k", "k", "nobody")
	assert.EqualValues(t, 2, n)
	name, _ := db.Type(ctx, "k")
	assert.Equal(t, "string", name)
	name, _ = db.Type(ctx, "nobody")
	assert.Equal(t, "none", name)
	renamed, err := db.RenameNX(ctx, "k", "f")
	require.NoError(t, err)
	assert.False(t, renamed)
	require.NoError(t, db.Rename(ctx, "k", "moved"))
	_, err = db.TTL(ctx, "k")
	assert.ErrorIs(t, err, keel.ErrNotFound)

	set, _ := db.Expire(ctx, "moved", time.Hour)
	assert.True(t, set)
	when, _ := db.ExpireTime(ctx, "moved")
	assert.WithinDuration(t, time.Now().Add(time.Hour), when, time.Minute)
	set, _ = db.Expire(ctx, "moved", time.Minute, keel.ExpireGT)
	assert.False(t, set, "GT leaves a later expiry")
	removed, _ := db.Persist(ctx, "moved")
	assert.True(t, removed)
	set, _ = db.Expire(ctx, "moved", -time.Second)
	assert.True(t, set)
	_, err = db.Get(ctx, "moved")
	assert.ErrorIs(t, err, keel.ErrNotFound, "an expiry in the past deletes the key")

	deleted, _ := db.Del(ctx, "f", "nobody")
	assert.EqualValues(t, 1, deleted)
	require.NoError(t, db.FlushDB(ctx))
	size, _ := db.DBSize(ctx)
	assert.Zero(t, size)

	reply, err := db.Do(ctx, "MGET", "a", "b")
	require.NoError(t, err)
	assert.Equal(t, []any{nil, nil}, reply)
	reply, _ = db.Do(ctx, "PING")
	assert.Equal(t, "PONG", reply)
	_, err = db.Do(ctx)
	assert.Error(t, err)
	assert.True(t, errors.Is(err, err))
}
