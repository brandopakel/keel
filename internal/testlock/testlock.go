// Package testlock keeps tests in different test processes from running at
// once when, together, they would hold more files than a local validation run
// has room for. `go test ./...` runs packages side by side, each in a process
// of its own, so nothing inside one package's tests can keep two packages'
// tests apart; a lock file the processes share can.
//
// Only tests import this package. Nothing the server is built from does, so
// it is not in the server's binary (`go list -deps ./cmd/keel`).
package testlock

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// diskHeavyWait bounds how long a test waits for the disk-heavy lock: well
// above the longest hold, a few seconds, and well under go test's default
// ten-minute package timeout, so a lock held too long fails the test that
// waits rather than hanging it.
const diskHeavyWait = 3 * time.Minute

// diskHeavyWhy is what the lock is for, as a failure explains it.
const diskHeavyWhy = "the lock keeps keel's largest test writers apart: core's " +
	"TestRewriteStallProfile (about 106 MiB of logs) and each subtest of cmd/keel's " +
	"TestRejectedCollectionPopsPreservePipelineAndRestart (about 65 MiB). " +
	"Side by side, with the Go build cache, a whole-suite run would pass the local " +
	"validation wrapper's 512 MiB budget (scripts/run-local-validation.py)"

// DiskHeavyPath is the disk-heavy lock's file, in the temporary directory
// every test process of one run shares.
func DiskHeavyPath() string {
	return filepath.Join(os.TempDir(), "keel-test-disk-heavy.lock")
}

// HoldDiskHeavy holds the disk-heavy lock from now until t ends, waiting for
// any other test that holds it. A test takes this lock and no other, so two
// holders cannot deadlock.
//
// Where the platform has no flock (not unix), it does nothing: the tests that
// take it then run as they did before it existed.
func HoldDiskHeavy(t testing.TB) {
	t.Helper()
	hold(t, DiskHeavyPath(), diskHeavyWhy, diskHeavyWait)
}

// hold takes the lock at path, failing t if another test holds it for longer
// than wait, and releases it when t ends. It logs when it had to wait, and
// when it took and gave back the lock, so a run's log shows which tests the
// lock kept apart.
func hold(t testing.TB, path, why string, wait time.Duration) {
	t.Helper()
	asked := time.Now()
	release, err := acquire(path, wait)
	if err != nil {
		t.Fatalf("%v; %s", err, why)
		return
	}
	held := time.Now()
	if waited := held.Sub(asked); waited >= 10*time.Millisecond {
		t.Logf("waited %s for %s, which another test held", waited.Round(time.Millisecond), path)
	}
	t.Logf("holding %s from %s", path, held.UTC().Format("15:04:05.000"))
	t.Cleanup(func() {
		release()
		t.Logf("released %s at %s, after %s", path, time.Now().UTC().Format("15:04:05.000"),
			time.Since(held).Round(time.Millisecond))
	})
}
