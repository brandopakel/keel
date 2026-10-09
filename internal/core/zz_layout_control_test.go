package core

import "testing"

// TestLayoutControl is a layout control, never merged: it never runs past
// its skip, and keeps the pads linked without calling them.
func TestLayoutControl(t *testing.T) {
	t.Skip("layout control")
	for _, pad := range layoutPads {
		pad()
	}
}
