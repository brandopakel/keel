package core

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"memkv/internal/constant"
)

// TestCmdSetReadsTheExpiryKeyword.
//
// The keyword used to be skipped: args[3] was read as seconds whatever args[2]
// said. PX therefore set a TTL a thousand times longer than asked for, and any
// word at all was accepted in the position - both silently, since the reply was
// OK either way.
func TestCmdSetReadsTheExpiryKeyword(t *testing.T) {
	ResetStores()

	assert.EqualValues(t, "OK", mustDecode(t, cmdSET([]string{"s", "v", "EX", "100"})))
	// Within one second, not exactly 100: TTL reports whole seconds and rounds
	// down, so a millisecond spent between the two commands shows as 99.
	assert.InDelta(t, 100, mustDecode(t, cmdTTL([]string{"s"})), 1)

	// 100 milliseconds is under a second, so TTL in whole seconds is 0 - not
	// the 100 it reported when PX was read as EX.
	assert.EqualValues(t, "OK", mustDecode(t, cmdSET([]string{"ms", "v", "PX", "100"})))
	assert.EqualValues(t, 0, mustDecode(t, cmdTTL([]string{"ms"})))

	// Lower case is the same keyword.
	assert.EqualValues(t, "OK", mustDecode(t, cmdSET([]string{"lower", "v", "px", "100"})))
	assert.EqualValues(t, 0, mustDecode(t, cmdTTL([]string{"lower"})))
}

func TestCmdSetRejectsWhatItDoesNotImplement(t *testing.T) {
	ResetStores()

	for _, bad := range [][]string{
		{"k", "v", "ZZ", "100"},
		{"k", "v", "KEEPTTL", "100"},
		{"k", "v", "EXAT", "100"},
	} {
		res, _ := Decode(cmdSET(bad))
		assert.Contains(t, res, "syntax error", "SET %v must be refused, not guessed at", bad)
		assert.Equal(t, constant.RespNil, cmdGET([]string{"k"}), "and must not have written anything")
	}
}

func TestCmdSetRejectsAnExpiryThatHasAlreadyPassed(t *testing.T) {
	ResetStores()
	for _, bad := range [][]string{{"k", "v", "EX", "0"}, {"k", "v", "PX", "-1"}} {
		res, _ := Decode(cmdSET(bad))
		assert.Contains(t, res, "invalid expire time", "SET %v", bad)
	}
}

// TestCmdSetWithoutExpiryIsUnchanged guards the common path.
func TestCmdSetWithoutExpiryIsUnchanged(t *testing.T) {
	ResetStores()
	assert.EqualValues(t, "OK", mustDecode(t, cmdSET([]string{"plain", "value"})))
	assert.EqualValues(t, "value", mustDecode(t, cmdGET([]string{"plain"})))
	assert.EqualValues(t, -1, mustDecode(t, cmdTTL([]string{"plain"})), "no TTL means no expiry")
}
