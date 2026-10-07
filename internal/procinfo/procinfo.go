// Package procinfo reads what the operating system knows about this process
// and its host, for INFO: the system's name, the processor time used, the
// resident memory and the host's physical memory.
//
// Each reader says whether this platform can answer, so that INFO leaves a
// field out rather than report a value it made up. Linux answers everything,
// which is where Keel is deployed; other Unix systems answer the system's name
// and processor time.
package procinfo

import "time"

// CPU is the processor time this process has used, and the time its children
// that have been waited for have used, in user and system mode.
type CPU struct {
	User, Sys, ChildrenUser, ChildrenSys time.Duration
}

// cString is the text of a NUL-terminated byte array, as uname fills them.
func cString[T int8 | uint8](b []T) string {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c == 0 {
			break
		}
		out = append(out, byte(c))
	}
	return string(out)
}
