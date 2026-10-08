//go:build !unix

package procinfo

// CPUTime is not available on this platform.
func CPUTime() (CPU, bool) { return CPU{}, false }
