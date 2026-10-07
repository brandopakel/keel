//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd || windows)

package core

import (
	"fmt"
	"runtime"
)

// logLock is never held where there is no lock to take.
type logLock struct{}

// lockFile reports that this platform has no lock the log can take: AIX,
// Solaris, illumos, Plan 9, js and wasip1, none of which runs the server. The
// log is opened without one.
func lockFile(path string) (*logLock, error) {
	return nil, fmt.Errorf("%s: %w (no file locks on %s)", path, errLockUnsupported, runtime.GOOS)
}

func (l *logLock) release() error { return nil }
