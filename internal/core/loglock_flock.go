//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package core

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// logLock is a held flock on the lock file's open descriptor.
type logLock struct{ file *os.File }

// lockFile takes an exclusive flock on path, creating the file if need be,
// without waiting for whoever holds it.
func lockFile(path string) (*logLock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	switch {
	case err == nil:
		return &logLock{file: f}, nil
	case errors.Is(err, syscall.EWOULDBLOCK):
		err = ErrLocked
	case errors.Is(err, syscall.ENOTSUP), errors.Is(err, syscall.EOPNOTSUPP), errors.Is(err, syscall.ENOLCK):
		err = fmt.Errorf("%s: %w (%v)", path, errLockUnsupported, err)
	default:
		err = &os.PathError{Op: "flock", Path: path, Err: err}
	}
	_ = f.Close()
	return nil, err
}

// release lets go of the lock: closing the descriptor releases its flock.
func (l *logLock) release() error { return l.file.Close() }
