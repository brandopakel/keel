//go:build unix

package testlock

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestASecondHolderWaitsForTheFirst: the lock keeps two holders apart, and
// the second takes it as soon as the first lets go.
func TestASecondHolderWaitsForTheFirst(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "keel-test.lock")
	release, err := acquire(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		release()
	}()
	asked := time.Now()
	second, err := acquire(path, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer second()
	if waited := time.Since(asked); waited < 150*time.Millisecond {
		t.Fatalf("the second holder took the lock after %s, while the first still held it", waited)
	}
}

// failureRecorder is a test that records a failure rather than stopping.
type failureRecorder struct {
	testing.TB
	failure string
}

func (r *failureRecorder) Fatalf(format string, args ...any) {
	r.failure = fmt.Sprintf(format, args...)
}

// TestAHolderThatStaysTooLongFailsTheWaiterClearly: a waiter gives up at its
// deadline, rather than hanging, and says which lock and why it exists.
func TestAHolderThatStaysTooLongFailsTheWaiterClearly(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "keel-test.lock")
	release, err := acquire(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	waiter := &failureRecorder{TB: t}
	hold(waiter, path, "the reason the lock exists", 100*time.Millisecond)
	for _, want := range []string{path, "for more than 100ms", "the reason the lock exists"} {
		if !strings.Contains(waiter.failure, want) {
			t.Fatalf("the failure %q does not say %q", waiter.failure, want)
		}
	}
}

// TestAKilledHolderLeavesTheLockFree: the kernel releases a dead process's
// flock, so a test process killed while it holds the lock leaves nothing for
// the next test to wait on.
func TestAKilledHolderLeavesTheLockFree(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "keel-test.lock")
	child := exec.Command(os.Args[0], "-test.run=^TestHelperProcessHoldsTheLock$")
	child.Env = append(os.Environ(), "KEEL_TESTLOCK_HOLD="+path)
	out, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || line != "held\n" {
		_ = child.Process.Kill()
		t.Fatalf("the holder did not take the lock: %q, %v", line, err)
	}
	if _, err := acquire(path, 100*time.Millisecond); err == nil {
		_ = child.Process.Kill()
		t.Fatal("the lock was free while another process held it")
	}
	if err := child.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	release, err := acquire(path, 5*time.Second)
	if err != nil {
		t.Fatalf("a killed holder left the lock held: %v", err)
	}
	release()
}

// TestHelperProcessHoldsTheLock is the holder TestAKilledHolderLeavesTheLockFree
// starts, in a process of its own; run on its own it does nothing.
func TestHelperProcessHoldsTheLock(t *testing.T) {
	path := os.Getenv("KEEL_TESTLOCK_HOLD")
	if path == "" {
		return
	}
	if _, err := acquire(path, time.Second); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	fmt.Println("held")
	time.Sleep(time.Minute)
	os.Exit(0)
}
