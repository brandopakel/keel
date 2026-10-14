package server

import (
	"errors"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestClientLimitFitsTheProcesssFileLimit: clients that fit the soft limit
// need nothing; more are fitted by raising it towards the hard limit; more
// than either allows are refused in Redis's words, with the most that fit.
func TestClientLimitFitsTheProcesssFileLimit(t *testing.T) {
	limit := func(cur, max uint64, refuse bool) (func(*syscall.Rlimit) error, func(*syscall.Rlimit) error, *uint64) {
		set := cur
		return func(l *syscall.Rlimit) error { *l = syscall.Rlimit{Cur: cur, Max: max}; return nil },
			func(l *syscall.Rlimit) error {
				if refuse {
					return errors.New("refused")
				}
				set = l.Cur
				return nil
			}, &set
	}
	get, set, now := limit(1024, 4096, false)
	require.Empty(t, fitClientLimit(992, get, set))
	require.Equal(t, uint64(1024), *now, "fits: nothing raised")
	require.Empty(t, fitClientLimit(2000, get, set))
	require.Equal(t, uint64(2032), *now, "raised to fit")
	get, set, now = limit(1024, 4096, false)
	require.Equal(t, "The operating system is not able to handle the specified number of clients, try with 4064",
		fitClientLimit(10000, get, set))
	require.Equal(t, uint64(4096), *now, "raised as far as it goes")
	get, set, _ = limit(1024, 4096, true)
	require.Equal(t, "The operating system is not able to handle the specified number of clients, try with 992",
		fitClientLimit(2000, get, set))
	broken := func(*syscall.Rlimit) error { return errors.New("unreadable") }
	require.Equal(t, "The operating system is not able to handle the specified number of clients, try with 992",
		fitClientLimit(2000, broken, broken), "an unreadable limit is taken as 1024, as Redis takes it")
	require.Empty(t, fitClientLimit(992, broken, broken))
	require.Empty(t, checkClientLimit(10), "the process's own limit")
}
