package server

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/brandopakel/keel/internal/core"
)

// TestMigratingFromTheLegacyLogSurvivesASecondRestart.
//
// The fallback on its own does not save the data, it delays losing it. Start
// one reads memkv-master.aof and opens an empty keel-master.aof; start two sees
// keel-master.aof present, prefers it, and replays only what was written after
// the migration. Everything that lived solely in the old log is gone, with the
// old log still sitting there looking like a backup.
//
// Reproduced before it was fixed: `legacy-k` was readable after the first
// restart and absent after the second.
func TestMigratingFromTheLegacyLogSurvivesASecondRestart(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "memkv-master.aof")
	current := filepath.Join(dir, "keel-master.aof")

	assert.NoError(t, os.WriteFile(legacy,
		[]byte("*3\r\n$3\r\nSET\r\n$8\r\nlegacy-k\r\n$5\r\nvalue\r\n"), 0o644))

	options := core.Options{AppendOnly: true, AppendFilename: current}
	keys := func(e *core.Engine) string {
		var reply bytes.Buffer
		assert.NoError(t, e.EvalAndResponse(&core.Command{Cmd: "DBSIZE"}, &reply))
		return reply.String()
	}

	// First start: reads the legacy log, then writes what it read into the
	// current one before anything else appends to it.
	e := newTestEngine(t, options)
	assert.NoError(t, startAOF(e, legacy))
	assert.Equal(t, ":1\r\n", keys(e), "the legacy key is here after one restart")
	assert.NoError(t, e.EvalAndResponse(
		&core.Command{Cmd: "SET", Args: []string{"new-k", "added"}}, &bytes.Buffer{}))
	assert.NoError(t, e.FlushAOF())
	assert.NoError(t, e.CloseAOF())

	// Second start, on an engine of its own as a restarted server's is: the
	// current file now exists and takes precedence.
	e = newTestEngine(t, options)
	assert.NoError(t, startAOF(e, legacy))
	assert.Equal(t, ":2\r\n", keys(e),
		"the key that lived only in the legacy log has to survive the file swap")
}

// startAOF runs the log's startup on e as the server's does, but with the
// legacy log looked for at legacy rather than at the server's name.
func startAOF(e *core.Engine, legacy string) error {
	return e.StartAOF(context.Background(), legacy)
}

// legacyLog writes a log of n keys at path, as the server wrote before the
// rename.
func legacyLog(t *testing.T, path string, n int) {
	t.Helper()
	var b bytes.Buffer
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("legacy-%d", i)
		fmt.Fprintf(&b, "*3\r\n$3\r\nSET\r\n$%d\r\n%s\r\n$5\r\nvalue\r\n", len(key), key)
	}
	require.NoError(t, os.WriteFile(path, b.Bytes(), 0o644))
}

// migrateInAProcess starts a process of its own that migrates the legacy log
// in dir, as the server's startup does, with env added to its environment.
func migrateInAProcess(t *testing.T, dir string, env ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcessMigrates$")
	cmd.Env = append(append(os.Environ(), "KEEL_SERVER_MIGRATE_DIR="+dir), env...)
	return cmd
}

// migratedKeys starts the server's log in dir again, here, and counts the
// keys it comes up with.
func migratedKeys(t *testing.T, dir string) string {
	t.Helper()
	e := newTestEngine(t, core.Options{AppendOnly: true, AppendFilename: filepath.Join(dir, "keel-master.aof")})
	require.NoError(t, startAOF(e, filepath.Join(dir, "memkv-master.aof")))
	var reply bytes.Buffer
	require.NoError(t, e.EvalAndResponse(&core.Command{Cmd: "DBSIZE"}, &reply))
	return reply.String()
}

// TestAMigrationThatFailsLeavesTheLegacyLogToReplay: a migration stopped by a
// failure before the new log holds the whole keyspace - here the rewrite into
// it, which a file-size limit fails - leaves no log under the new name, so the
// next start replays the legacy log again and loses nothing. It used to open
// the new log under its name first, and the next start preferred that empty
// log to the intact legacy one.
func TestAMigrationThatFailsLeavesTheLegacyLogToReplay(t *testing.T) {
	dir := t.TempDir()
	legacyLog(t, filepath.Join(dir, "memkv-master.aof"), 100)
	out, err := migrateInAProcess(t, dir, "KEEL_SERVER_FILE_LIMIT=1024").CombinedOutput()
	require.Error(t, err, "the migration must fail under the file-size limit:\n%s", out)
	require.Contains(t, string(out), "migrating", "%s", out)
	_, err = os.Stat(filepath.Join(dir, "keel-master.aof"))
	require.ErrorIs(t, err, os.ErrNotExist, "a failed migration leaves no log under the new name")
	require.Equal(t, ":100\r\n", migratedKeys(t, dir), "the next start replays the legacy log again")
}

// TestAMigrationKilledInItsWindowLosesNothing: a process killed while it
// writes the legacy keyspace into the new log - the window between creating
// that log and filling it - leaves either no log under the new name or a
// complete one, so the next start comes up with every legacy key.
func TestAMigrationKilledInItsWindowLosesNothing(t *testing.T) {
	dir := t.TempDir()
	const keys = 150000
	legacyLog(t, filepath.Join(dir, "memkv-master.aof"), keys)
	child := migrateInAProcess(t, dir)
	require.NoError(t, child.Start())
	exited := make(chan error, 1)
	go func() { exited <- child.Wait() }()
	defer func() {
		_ = child.Process.Kill()
		<-exited
		exited <- nil
	}()
	// The rewrite's own file appears once the keyspace is being written into
	// the new log, and goes when it is renamed: kill the process the moment
	// it is seen. A process that finishes before it is seen has missed the
	// window, which loses nothing either; the keys are checked all the same.
	deadline := time.Now().Add(time.Minute)
	for {
		require.True(t, time.Now().Before(deadline), "the migration neither began its rewrite nor finished")
		select {
		case err := <-exited:
			require.NoError(t, err, "the migration failed")
			exited <- err
			t.Log("the migration finished before its window was seen; nothing was killed in it")
			require.Equal(t, fmt.Sprintf(":%d\r\n", keys), migratedKeys(t, dir))
			return
		default:
		}
		matches, err := filepath.Glob(filepath.Join(dir, "*.rewrite"))
		require.NoError(t, err)
		if len(matches) > 0 {
			break
		}
		time.Sleep(200 * time.Microsecond)
	}
	require.NoError(t, child.Process.Kill())
	err := <-exited
	exited <- err
	require.Error(t, err, "the process was killed inside its window")
	require.Equal(t, fmt.Sprintf(":%d\r\n", keys), migratedKeys(t, dir))
}

// TestHelperProcessMigrates is the migration tests' other process: it starts
// the server's log in the directory it is given, which holds a legacy log,
// under a file-size limit if it is given one, and exits 0 if that worked. In
// the test process it returns at once.
func TestHelperProcessMigrates(t *testing.T) {
	dir := os.Getenv("KEEL_SERVER_MIGRATE_DIR")
	if dir == "" {
		return
	}
	if limit := os.Getenv("KEEL_SERVER_FILE_LIMIT"); limit != "" {
		n, err := strconv.ParseUint(limit, 10, 64)
		if err != nil {
			panic(err)
		}
		if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: n, Max: n}); err != nil {
			panic(err)
		}
	}
	e, err := core.NewEngine(core.Options{AppendOnly: true, AppendFilename: filepath.Join(dir, "keel-master.aof")})
	if err != nil {
		panic(err)
	}
	if err := startAOF(e, filepath.Join(dir, "memkv-master.aof")); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	os.Exit(0)
}
