package core

import "testing"

// TestLayoutControl is a layout control, never merged: it never runs past its
// skip, and keeps the pads linked.
func TestLayoutControl(t *testing.T) {
	t.Skip("layout control")
	layoutPad()
	layoutTestPad()
}

// layoutTestPad moves the code after the package's tests by one more 32-byte
// slot.
//
//go:noinline
func layoutTestPad() {}
