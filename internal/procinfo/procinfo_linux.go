package procinfo

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// OS names the system as Redis's INFO does: the kernel's name, its release and
// the machine, as uname reports them ("Linux 6.17.0-1022-azure x86_64").
func OS() string {
	var u syscall.Utsname
	if err := syscall.Uname(&u); err != nil {
		return "Linux"
	}
	return cString(u.Sysname[:]) + " " + cString(u.Release[:]) + " " + cString(u.Machine[:])
}

// ResidentBytes is the memory of this process that is resident in RAM, from
// the second field of /proc/self/statm, in pages - the same source Redis reads
// for used_memory_rss.
func ResidentBytes() (uint64, bool) {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0, false
	}
	pages, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return pages * uint64(os.Getpagesize()), true
}

// PhysicalBytes is the host's physical memory.
func PhysicalBytes() (uint64, bool) {
	var info syscall.Sysinfo_t
	if err := syscall.Sysinfo(&info); err != nil {
		return 0, false
	}
	return uint64(info.Totalram) * uint64(info.Unit), true
}
