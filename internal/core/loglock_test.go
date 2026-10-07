package core

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestASecondInstanceOfALogIsRefused: two engines in one process conflict
// over a log as two processes do. The second Open is refused before it reads
// anything, and leaves the first's log as it was; once the first is closed,
// the log can be opened again.
func TestASecondInstanceOfALogIsRefused(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "keel.aof")
	o := Options{AppendOnly: true, AppendFilename: path, Fsync: FsyncAlways}
	first := openTestEngine(t, o)
	runOn(t, first, "SET", "k", "first")
	require.NoError(t, first.FlushAOF())
	before := filesIn(t, dir)

	second, err := Open(context.Background(), o)
	require.ErrorIs(t, err, ErrLocked)
	assert.Nil(t, second)
	assert.EqualError(t, err, "appendonly: log in use by another instance: "+path+".lock")
	assert.Equal(t, before, filesIn(t, dir), "the refused instance read and wrote nothing")
	assert.Equal(t, "first", runOn(t, first, "GET", "k"), "and the first goes on")

	// Another log beside it is another lock.
	openTestEngine(t, Options{AppendOnly: true, AppendFilename: filepath.Join(dir, "other.aof")})

	require.NoError(t, first.Close())
	again := openTestEngine(t, o)
	assert.Equal(t, "first", runOn(t, again, "GET", "k"))
}

// TestCloseReleasesTheLogsLockAndLeavesItsFile: the lock file stays, empty,
// and holds nothing once its engine is closed. An engine with no log takes no
// lock.
func TestCloseReleasesTheLogsLockAndLeavesItsFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "keel.aof")
	e := openTestEngine(t, Options{AppendOnly: true, AppendFilename: path})
	assert.Contains(t, filesIn(t, dir), "keel.aof.lock")
	require.NoError(t, e.Close())
	assert.Equal(t, "", filesIn(t, dir)["keel.aof.lock"], "the file is left behind, empty")

	held, err := lockLog(path)
	require.NoError(t, err, "and unlocked")
	require.NoError(t, held.release())

	noLog := t.TempDir()
	openTestEngine(t, Options{AppendFilename: filepath.Join(noLog, "keel.aof")})
	assert.Empty(t, filesIn(t, noLog))
}

// TestALogHeldByAnotherProcess: a process that holds a log keeps every other
// process off it, and when it is killed - a crash, with no Close - the kernel
// releases its lock, so the lock file it leaves behind is no stale lock.
func TestALogHeldByAnotherProcess(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "keel.aof")

	holder := exec.Command(os.Args[0], "-test.run=^TestHelperProcessHoldsALog$")
	holder.Env = append(os.Environ(), "KEEL_CORE_HOLD_LOG="+path)
	stdin, err := holder.StdinPipe()
	require.NoError(t, err)
	stdout, err := holder.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, holder.Start())
	killed := false
	t.Cleanup(func() {
		if !killed {
			_ = holder.Process.Kill()
			_ = holder.Wait()
		}
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "held\n", line, "the holder did not open the log")

	e, err := Open(context.Background(), Options{AppendOnly: true, AppendFilename: path})
	require.ErrorIs(t, err, ErrLocked)
	assert.Nil(t, e)

	require.NoError(t, holder.Process.Kill())
	_ = holder.Wait()
	killed = true
	_ = stdin.Close()

	e = openTestEngine(t, Options{AppendOnly: true, AppendFilename: path})
	assert.Equal(t, "held", runOn(t, e, "GET", "holder"), "the holder's write is there, and its lock is not")
}

// TestHelperProcessHoldsALog is TestALogHeldByAnotherProcess's other process:
// it opens the log it is given, writes to it, says so, and holds the log until
// it is killed, or its input closes. In the test process it returns at once.
func TestHelperProcessHoldsALog(t *testing.T) {
	path := os.Getenv("KEEL_CORE_HOLD_LOG")
	if path == "" {
		return
	}
	e, err := Open(context.Background(), Options{AppendOnly: true, AppendFilename: path, Fsync: FsyncAlways})
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	runOn(t, e, "SET", "holder", "held")
	if err := e.FlushAOF(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	fmt.Println("held")
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}
