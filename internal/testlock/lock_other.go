//go:build !unix

package testlock

import "time"

// acquire does nothing where there is no flock: the tests that take the lock
// run unserialized, as they did before it existed. CI and local validation
// run on Linux and macOS.
func acquire(string, time.Duration) (func(), error) {
	return func() {}, nil
}
