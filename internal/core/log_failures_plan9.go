//go:build plan9

package core

// errnoText has no errno to give on Plan 9, whose system calls fail with
// strings, so a failure there keeps Go's text.
func errnoText(error) (string, bool) { return "", false }
