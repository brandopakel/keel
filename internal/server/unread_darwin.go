//go:build darwin

package server

import (
	"syscall"
	"unsafe"
)

// fionread asks how many bytes a socket holds unread. Darwin spells it
// _IOR('f', 127, int).
const fionread = 0x4004667F

// unreadBytes is how many bytes the kernel is holding for this descriptor
// that the process has not read.
//
// It answers the one question a lost readiness registration raises. Such a
// connection owes nothing and holds nothing: the request was never seen, so
// there are no parsed commands, no reply and no partial buffer, and every
// other check in the sweep passes it over. The bytes themselves are the
// evidence, and only the kernel has them.
//
// false means the question could not be asked, which is not evidence of
// anything and is never treated as a fault.
func unreadBytes(fd int) (int, bool) {
	var n int
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), fionread, uintptr(unsafe.Pointer(&n)))
	if errno != 0 {
		return 0, false
	}
	return n, true
}
