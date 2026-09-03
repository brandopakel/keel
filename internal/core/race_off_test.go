//go:build !race

package core

// raceEnabled reports whether the binary was built with -race.
//
// The race detector is not a neutral observer of a test that measures time: it
// instruments every memory access, which inflates the durations the rewrite
// stall profile asserts on by roughly four times. A timing test under -race
// measures the detector.
const raceEnabled = false
