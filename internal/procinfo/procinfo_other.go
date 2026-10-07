//go:build !linux && !darwin

package procinfo

import "runtime"

// OS names the system by Go's names for it, where it has no uname to ask.
func OS() string { return runtime.GOOS + " " + runtime.GOARCH }

// ResidentBytes is not available on this platform.
func ResidentBytes() (uint64, bool) { return 0, false }

// PhysicalBytes is not available on this platform.
func PhysicalBytes() (uint64, bool) { return 0, false }
