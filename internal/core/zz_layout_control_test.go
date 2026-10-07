package core

import "testing"

// TestLayoutControl is a layout control, never merged: it never runs past its
// skip, and is here only to move the code after it - the generic stores'
// methods, which the package emits last - back to the 64-byte phase they had
// before this part.
func TestLayoutControl(t *testing.T) {
	t.Skip("layout control")
	layoutPad()
}
