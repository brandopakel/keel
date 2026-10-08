package core

import "testing"

// TestLayoutControl is a layout control, never merged: it never runs past its
// skip, and keeps layoutPad linked.
func TestLayoutControl(t *testing.T) {
	t.Skip("layout control")
	layoutPad()
}
