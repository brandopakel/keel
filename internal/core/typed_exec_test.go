package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// txCall is a TxCall of name that runs method and keeps its error in err.
func txCall(name string, err *error, method func(context.Context) error) TxCall {
	*err = errors.New("not run")
	return TxCall{Name: name, Run: func(ctx context.Context) { *err = method(ctx) }}
}

// TestExecIsLoggedAsEXECsBlock: Exec's calls are logged as EXEC logs a
// client's block - each command as itself, inside one MULTI ... EXEC frame,
// a failed command and a read not at all - and the block replays. They count
// as the commands a client sends for them, MULTI and EXEC among them; a
// failing call fails alone; and a call sees what the calls before it wrote.
func TestExecIsLoggedAsEXECsBlock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "exec.aof")
	o := Options{AppendOnly: true, AppendFilename: path, Fsync: FsyncAlways}
	e := openTestEngine(t, o)
	_, err := e.Set(ctx, "text", "x", SetOptions{})
	require.NoError(t, err)
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	stats, err := e.Stats(ctx)
	require.NoError(t, err)

	var sum int64
	var value string
	errs := make([]error, 6)
	at := time.UnixMilli(4102444800123)
	calls := []TxCall{
		txCall("SET", &errs[0], func(ctx context.Context) error {
			_, err := e.Set(ctx, "a", "1", SetOptions{ExpireAt: at})
			return err
		}),
		txCall("GET", &errs[1], func(ctx context.Context) (err error) { value, _, err = e.Get(ctx, "a"); return err }),
		txCall("INCR", &errs[2], func(ctx context.Context) error { _, err := e.IncrBy(ctx, "INCR", "text", 1); return err }),
		txCall("INCRBY", &errs[3], func(ctx context.Context) (err error) { sum, err = e.IncrBy(ctx, "INCRBY", "a", 4); return err }),
		txCall("MSET", &errs[4], func(ctx context.Context) error { return e.MSet(ctx, []string{"x", "1", "y", "2"}) }),
		txCall("SET", &errs[5], func(ctx context.Context) error {
			_, err := e.Set(ctx, "bad", "v", SetOptions{NX: true, XX: true})
			return err
		}),
	}
	require.NoError(t, e.Exec(ctx, calls))
	assert.NoError(t, errs[0])
	assert.NoError(t, errs[1])
	assert.Equal(t, "1", value, "a call sees what the calls before it in the block wrote")
	assert.Equal(t, CategoryNotInteger, CategoryOf(errs[2]))
	assert.NoError(t, errs[3])
	assert.EqualValues(t, 5, sum, "the call after the failure ran")
	assert.NoError(t, errs[4])
	assert.Equal(t, errSyntax, errs[5], "a refused argument fails its call alone, and runs nothing")

	after, err := e.Stats(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 5+2, after.Commands-stats.Commands, "five calls ran, with MULTI and EXEC")
	assert.EqualValues(t, 1, after.ErrorReplies-stats.ErrorReplies, "INCR's refusal")

	body, err := os.ReadFile(path)
	require.NoError(t, err)
	want := string(before) + "*1\r\n$5\r\nMULTI\r\n" +
		string(appendCommand(nil, "SET", "a", "1")) + string(appendCommand(nil, "PEXPIREAT", "a", "4102444800123")) +
		string(appendCommand(nil, "INCRBY", "a", "4")) + string(appendCommand(nil, "MSET", "x", "1", "y", "2")) +
		"*1\r\n$4\r\nEXEC\r\n"
	assert.Equal(t, want, string(body))

	// A block of reads, or of writes that change nothing, logs nothing.
	var size int64
	require.NoError(t, e.Exec(ctx, []TxCall{
		txCall("DBSIZE", &errs[0], func(ctx context.Context) (err error) { size, err = e.DBSize(ctx); return err }),
		txCall("DEL", &errs[1], func(ctx context.Context) error { _, err := e.Del(ctx, "nobody"); return err }),
	}))
	assert.EqualValues(t, 4, size)
	again, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(body), string(again))

	// A call other than the one queued is refused, and runs nothing.
	require.NoError(t, e.Exec(ctx, []TxCall{
		txCall("GET", &errs[0], func(ctx context.Context) error { _, err := e.Del(ctx, "a"); return err }),
	}))
	assert.ErrorContains(t, errs[0], "queued as GET made a typed call of DEL")

	require.NoError(t, e.Close())
	reopened := openTestEngine(t, o)
	got, _, err := reopened.Get(ctx, "a")
	require.NoError(t, err)
	assert.Equal(t, "5", got)
	expiry, err := reopened.ExpireTime(ctx, "a")
	require.NoError(t, err)
	assert.EqualValues(t, 4102444800123, expiry)
	got, _, _ = reopened.Get(ctx, "y")
	assert.Equal(t, "2", got)
}

// TestExecRefusesTheWholeBlock: what Redis refuses EXEC for refuses Exec
// before any of its calls run - a closed engine, a done context, a log whose
// failed write is retried when the block writes, a replica when it holds a
// write - and a block of reads runs while the log retries.
func TestExecRefusesTheWholeBlock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := Options{AppendOnly: true, AppendFilename: filepath.Join(t.TempDir(), "exec.aof"), Fsync: FsyncEverySec}
	e := openTestEngine(t, o)
	var value string
	var getErr, setErr error
	get := func(key string) TxCall {
		value = ""
		return txCall("GET", &getErr, func(ctx context.Context) (err error) { value, _, err = e.Get(ctx, key); return err })
	}
	set := func(key string) TxCall {
		return txCall("SET", &setErr, func(ctx context.Context) error { _, err := e.Set(ctx, key, "v", SetOptions{}); return err })
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	assert.ErrorIs(t, e.Exec(cancelled, []TxCall{set("k")}), context.Canceled)
	assert.EqualError(t, setErr, "not run")

	disk := failDisk(e)
	_, err := e.Set(ctx, "before", "v", SetOptions{})
	require.NoError(t, err)
	disk.writes.Store(true)
	_, err = e.Set(ctx, "inflight", "v", SetOptions{})
	require.NoError(t, err, "the write in flight at the failure keeps its answer")
	eventually(t, e, "the flush failed", e.logRetrying)
	err = e.Exec(ctx, []TxCall{get("before"), set("denied")})
	assert.ErrorIs(t, err, ErrPersistence, "a block that writes is refused while the log retries")
	assert.EqualError(t, getErr, "not run", "nothing of it ran")
	require.NoError(t, e.Exec(ctx, []TxCall{get("before")}), "a block of reads runs")
	assert.NoError(t, getErr)
	assert.Equal(t, "v", value)
	disk.writes.Store(false)
	eventually(t, e, "the retried write succeeded", func() bool { return !e.logRetrying() })
	_, found, err := e.Get(ctx, "denied")
	require.NoError(t, err)
	assert.False(t, found)

	holding(e, func() {
		c := e.options
		c.ReplicaOf = "primary.test:6379"
		require.NoError(t, e.configure(c))
		e.replicaReady, e.replicaUpdated = true, time.Now()
	})
	stats, err := e.Stats(ctx)
	require.NoError(t, err)
	assert.ErrorIs(t, e.Exec(ctx, []TxCall{get("before"), set("replica")}), errReadOnlyReplica)
	assert.EqualError(t, getErr, "not run")
	after, err := e.Stats(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 1, after.ErrorReplies-stats.ErrorReplies)
	assert.EqualValues(t, 2, after.Commands-stats.Commands, "MULTI and EXEC, as the server counts them")
	holding(e, func() {
		assert.EqualValues(t, 1, e.totals.errorPrefixes["EXECABORT"], "counted as EXEC's refusal")
		c := e.options
		c.ReplicaOf = ""
		require.NoError(t, e.configure(c))
	})

	require.NoError(t, e.Close())
	assert.ErrorIs(t, e.Exec(ctx, []TxCall{get("before")}), ErrClosed)
}

// TestExecIsIsolated: no call runs between a block's calls, however many
// callers there are: a reader never sees the value a block writes and then
// overwrites.
func TestExecIsIsolated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := openTestEngine(t, Options{})
	_, err := e.Set(ctx, "k", "settled", SetOptions{})
	require.NoError(t, err)
	done := make(chan struct{})
	seen := make(chan string, 1)
	go func() {
		defer close(seen)
		for {
			select {
			case <-done:
				return
			default:
			}
			value, _, err := e.Get(ctx, "k")
			if err != nil || value != "settled" {
				seen <- value
				return
			}
		}
	}()
	var errs [2]error
	set := func(i int, value string) TxCall {
		return txCall("SET", &errs[i], func(ctx context.Context) error {
			_, err := e.Set(ctx, "k", value, SetOptions{})
			return err
		})
	}
	for range 2000 {
		require.NoError(t, e.Exec(ctx, []TxCall{set(0, "between"), set(1, "settled")}))
		require.NoError(t, errs[0])
		require.NoError(t, errs[1])
	}
	close(done)
	for value := range seen {
		t.Fatalf("a reader saw %q inside a block", value)
	}
}

// TestScanStepsAsSCANDoes: a walk of typed steps keeps the keys a walk of
// SCAN keeps, under each filter, and a step counts as one SCAN. Walks are
// compared whole, as distinct keys: a step also stops at a time target, so
// two walks from one cursor need not stop at the same key.
func TestScanStepsAsSCANDoes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := openTestEngine(t, Options{})
	for i := range 300 {
		_, err := e.Set(ctx, fmt.Sprintf("user:%s%c%d", strings.Repeat("x", i%3), 'a'+i%26, i), "v", SetOptions{})
		require.NoError(t, err)
	}
	holding(e, func() {
		for i := range 40 {
			runOn(t, e, "RPUSH", fmt.Sprintf("list:%d", i), "v")
		}
	})
	distinct := func(seen map[string]bool) []string {
		keys := make([]string, 0, len(seen))
		for key := range seen {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		return keys
	}
	for _, o := range []ScanOptions{{}, {Match: "user:x*"}, {Type: "list"}, {Type: "LIST", Count: 7}, {Match: "*a*", Type: "string", Count: 100}} {
		args := func(cursor string) []string {
			a := []string{cursor}
			if o.Match != "" {
				a = append(a, "MATCH", o.Match)
			}
			if o.Type != "" {
				a = append(a, "TYPE", o.Type)
			}
			if o.Count != 0 {
				a = append(a, "COUNT", strconv.Itoa(o.Count))
			}
			return a
		}
		typed, viaSCAN := map[string]bool{}, map[string]bool{}
		for cursor, first := uint64(0), true; first || cursor != 0; first = false {
			stats, err := e.Stats(ctx)
			require.NoError(t, err)
			var keys []string
			keys, cursor, err = e.Scan(ctx, cursor, o)
			require.NoError(t, err)
			after, err := e.Stats(ctx)
			require.NoError(t, err)
			require.EqualValues(t, 1, after.Commands-stats.Commands, "a step is a SCAN")
			for _, key := range keys {
				typed[key] = true
			}
		}
		for cursor := "0"; ; {
			var reply []any
			holding(e, func() { reply = runOn(t, e, "SCAN", args(cursor)...).([]any) })
			for _, key := range reply[1].([]any) {
				viaSCAN[key.(string)] = true
			}
			if cursor = reply[0].(string); cursor == "0" {
				break
			}
		}
		assert.Equal(t, distinct(viaSCAN), distinct(typed), "%+v", o)
		assert.NotEmpty(t, typed, "%+v", o)
	}
	_, _, err := e.Scan(ctx, 0, ScanOptions{Count: -1})
	assert.Equal(t, errSyntax, err)
	_, _, err = e.Scan(ctx, 0, ScanOptions{Count: scanMaxCount + 1})
	assert.Equal(t, errSyntax, err)
	// A pattern that uses up its work is SCAN's refusal.
	_, err = e.Set(ctx, strings.Repeat("a", 10000), "v", SetOptions{})
	require.NoError(t, err)
	costly := "*" + strings.Repeat("a", 1000) + "b"
	_, _, err = e.Scan(ctx, 0, ScanOptions{Match: costly, Count: scanMaxCount})
	assert.Equal(t, errScanMatchWork, err)
	holding(e, func() {
		assert.Equal(t, errScanMatchWork.Error(), runOn(t, e, "SCAN", "0", "MATCH", costly, "COUNT", strconv.Itoa(scanMaxCount)))
	})
}
