//go:build unix

package testlock

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// acquire takes an exclusive flock on path, creating the file if need be,
// and returns what releases it. It polls rather than blocking, so that it
// can give up once wait has passed.
//
// The kernel releases a flock when the process holding it exits, however it
// exits, so a test process that crashes or is killed while it holds the lock
// cannot leave it held: the file stays, empty, and the next test takes it.
func acquire(path string, wait time.Duration) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening the test lock %s: %w", path, err)
	}
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			_ = f.Close()
			return nil, fmt.Errorf("locking the test lock %s: %w", path, err)
		}
		if !time.Now().Before(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("another test held the test lock %s for more than %s", path, wait)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
