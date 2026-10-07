package core

import (
	"errors"
	"fmt"
)

// The log's lock: one instance per log, enforced with an advisory lock on a
// file beside it, path + ".lock", as the term file is path + ".term" (plan
// phase 3). It cannot be on the log's own descriptor, because a rewrite
// renames a new file over the log's path.
//
// The lock is a flock, not an fcntl lock, where there is a choice. An fcntl
// lock belongs to the process: a second engine on the same log in the same
// process would take it again, and closing any descriptor of the file would
// drop it. A flock belongs to the open file, so two engines in one process
// conflict as two processes do, and only closing the descriptor that took it
// releases it. On Windows the lock is the file opened with no sharing, which
// any second open fails, in the process or out of it.
//
// There is no stale lock to clear after a crash: the kernel releases a dead
// process's locks, and the file is left behind, empty and unlocked, for the
// next instance to take. The file is never removed, because removing a lock
// file races with whoever opens it next: one instance would hold a lock on the
// removed file and another on its replacement.

// lockFileSuffix names the log's lock file, beside the log.
const lockFileSuffix = ".lock"

// ErrLocked is the refusal to open a log another instance holds, in this
// process or another.
var ErrLocked = errors.New("log in use by another instance")

// errLockUnsupported marks a lock that cannot be taken here at all, because
// the platform or the filesystem has no locks: the log is opened without one.
var errLockUnsupported = errors.New("locks are not supported here")

// lockLog takes the lock beside the log at path. It returns an error wrapping
// ErrLocked when another instance holds it, one wrapping errLockUnsupported
// when locks cannot be taken there, and any other failure to create or open
// the lock file as it comes, naming the file.
func lockLog(path string) (*logLock, error) {
	lockPath := path + lockFileSuffix
	l, err := lockFile(lockPath)
	if errors.Is(err, ErrLocked) {
		return nil, fmt.Errorf("%w: %s", ErrLocked, lockPath)
	}
	return l, err
}
