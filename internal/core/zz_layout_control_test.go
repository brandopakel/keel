package core

import (
	"os/exec"
	"testing"
)

// TestLayoutControl is a layout control, never merged: it never runs past
// its skip. It links what follow-up 4's tests link from the standard library
// (exec.Cmd.Output and t.Log), and keeps the pads linked without calling them.
func TestLayoutControl(t *testing.T) {
	t.Skip("layout control")
	var c exec.Cmd
	_, _ = c.Output()
	t.Log("")
	for _, pad := range layoutPads {
		pad()
	}
}
