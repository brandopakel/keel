package server

import (
	"strconv"
	"syscall"
)

// reservedDescriptors is how many file descriptors Redis keeps for its own
// use beyond its clients' (CONFIG_MIN_RESERVED_FDS): the listening sockets,
// the log, the replication links.
const reservedDescriptors = 32

// checkClientLimit is Redis's updateMaxclients for CONFIG SET maxclients
// (adjustOpenFilesLimit): the process's limit on open files is raised, up to
// its hard limit, to hold n clients and the descriptors kept besides; if it
// cannot hold them, the value is refused in Redis's words, with the most it
// can hold.
func checkClientLimit(n int) string {
	return fitClientLimit(n, func(l *syscall.Rlimit) error { return syscall.Getrlimit(syscall.RLIMIT_NOFILE, l) },
		func(l *syscall.Rlimit) error { return syscall.Setrlimit(syscall.RLIMIT_NOFILE, l) })
}

// fitClientLimit is checkClientLimit with the limit read and set by get and
// set.
func fitClientLimit(n int, get, set func(*syscall.Rlimit) error) string {
	var limit syscall.Rlimit
	if err := get(&limit); err != nil {
		// Redis assumes 1024 when it cannot read the limit, and holds the
		// clients that fit in that.
		limit = syscall.Rlimit{Cur: 1024, Max: 1024}
		set = func(*syscall.Rlimit) error { return err }
	}
	need := uint64(n) + reservedDescriptors
	if limit.Cur >= need {
		return ""
	}
	// As far up as the hard limit allows; Redis keeps what it could raise it
	// to, even when that is not enough.
	best := limit.Cur
	raised := limit
	raised.Cur = min(limit.Max, need)
	if set(&raised) == nil {
		if raised.Cur >= need {
			return ""
		}
		best = raised.Cur
	}
	if best <= reservedDescriptors {
		best = reservedDescriptors + 1
	}
	return "The operating system is not able to handle the specified number of clients, try with " +
		strconv.FormatUint(best-reservedDescriptors, 10)
}
