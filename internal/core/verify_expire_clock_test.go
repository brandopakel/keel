package core

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A positive relative expiry is judged against the instant it was computed
// from, as Redis's expireGenericCommand judges both against one
// commandTimeSnapshot() (expire.c 652-661 and 726-754): PEXPIRE k 1 gives k an
// expiry and never deletes it, however long the command takes. The clock here
// moves 10 ms every time it is read, so a command that read it twice would
// find its own expiry already past. A race-instrumented runner once did
// exactly that between two real reads, and PEXPIRE k 1 logged a DEL.
func TestExpireJudgesItsExpiryAgainstOneInstant(t *testing.T) {
	e := newTestEngine(t, Options{})
	// An hour ahead, so the expiry is still in the future by the wall clock
	// the key's later reads use.
	base := time.Now().Add(time.Hour).UnixMilli()
	reads := 0
	e.commandClock = func() int64 {
		reads++
		return base + int64(10*(reads-1))
	}
	require.Equal(t, "OK", runOn(t, e, "SET", "k", "v"))

	require.EqualValues(t, 1, runOn(t, e, "PEXPIRE", "k", "1"))
	require.Equal(t, 1, reads, "one clock reading for the whole command")
	require.EqualValues(t, 1, runOn(t, e, "EXISTS", "k"), "the key keeps its expiry")
	owner, ok := e.space.OwnerOf("k")
	require.True(t, ok)
	at, has := owner.GetExpiry("k")
	require.True(t, has)
	require.EqualValues(t, base+1, at, "one millisecond after the command's instant")

	// A relative time that is not positive, or an absolute one at or before
	// the command's instant, still deletes the key at once, as Redis does.
	reads = 0
	require.EqualValues(t, 1, runOn(t, e, "PEXPIRE", "k", "0"))
	require.EqualValues(t, 0, runOn(t, e, "EXISTS", "k"))
	require.Equal(t, "OK", runOn(t, e, "SET", "k", "v"))
	require.EqualValues(t, 1, runOn(t, e, "PEXPIREAT", "k", "1"))
	require.EqualValues(t, 0, runOn(t, e, "EXISTS", "k"))
}
