package resp

import "os"

// A throwaway control for the paired command-path job, never merged: a
// function that is never called, sized to move the text after this package
// by one 32-byte unit, so that core's functions regain the 64-byte phase
// they have on the baseline (see the control branch's PR note).
func phasePad() int {
	n := 0
	for i := 0; i < len(os.Args); i++ {
		n += len(os.Args[i])
	}
	return n
}

func init() {
	if len(os.Args) > 1<<30 {
		phasePad()
	}
}
