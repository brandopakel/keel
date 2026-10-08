//go:build windows

package core

import (
	"errors"
	"os"
	"syscall"
)

// logLock is the lock file, held open with no sharing.
type logLock struct{ handle syscall.Handle }

// errorSharingViolation is ERROR_SHARING_VIOLATION: the file is open
// elsewhere in a way this open cannot share.
const errorSharingViolation syscall.Errno = 32

// lockFile opens path, creating it if need be, with no sharing: until the
// handle is closed, or its process ends, every other open of the file fails,
// in this process or another.
func lockFile(path string) (*logLock, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil,
		syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if errors.Is(err, errorSharingViolation) {
		return nil, ErrLocked
	}
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return &logLock{handle: h}, nil
}

// release lets go of the lock by closing the handle.
func (l *logLock) release() error { return syscall.CloseHandle(l.handle) }
