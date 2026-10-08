//go:build unix

package procinfo

import (
	"syscall"
	"time"
)

// CPUTime is the processor time this process and its waited-for children have
// used, from getrusage, as Redis's used_cpu_* fields report it.
func CPUTime() (CPU, bool) {
	var self, children syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &self) != nil || syscall.Getrusage(syscall.RUSAGE_CHILDREN, &children) != nil {
		return CPU{}, false
	}
	return CPU{
		User: time.Duration(self.Utime.Nano()), Sys: time.Duration(self.Stime.Nano()),
		ChildrenUser: time.Duration(children.Utime.Nano()), ChildrenSys: time.Duration(children.Stime.Nano()),
	}, true
}
