package procinfo

import "syscall"

// OS names the system as Redis's INFO does: the kernel's name, its release and
// the machine ("Darwin 25.5.0 arm64").
func OS() string {
	name, err1 := syscall.Sysctl("kern.ostype")
	release, err2 := syscall.Sysctl("kern.osrelease")
	machine, err3 := syscall.Sysctl("hw.machine")
	if err1 != nil || err2 != nil || err3 != nil {
		return "Darwin"
	}
	return name + " " + release + " " + machine
}

// ResidentBytes is not available here: macOS reports a process's resident
// memory through task_info, which Go reaches only with cgo.
func ResidentBytes() (uint64, bool) { return 0, false }

// PhysicalBytes is not available here without cgo either.
func PhysicalBytes() (uint64, bool) { return 0, false }
