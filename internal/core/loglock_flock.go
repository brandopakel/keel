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

// release lets go of the lock, and then of the descriptor. Closing the
// descriptor alone would not do: a process forked by any goroutine, between
// its fork and its exec, holds a copy of every descriptor, and a flock
// belongs to the open file they share. Unlocking releases it for every copy at
// once, so the log is free when release returns.
func (l *logLock) release() error {
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	if err := l.file.Close(); err != nil {
		return err
	}
	return unlockErr
}
