package core

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"memkv/internal/constant"
)

// TestCmdZScoreReportsTheScoreOfAMemberThatExists.
//
// ZSCORE had its success test inverted for the whole life of the command: it
// returned nil for every member that was present and a score of zero for every
// member that was not, which is the answer to the opposite question. ZADD and
// ZCARD both reported the member was there, so nothing else gave it away.
func TestCmdZScoreReportsTheScoreOfAMemberThatExists(t *testing.T) {
	ResetStores()

	assert.EqualValues(t, 2, mustDecode(t, cmdZADD([]string{"z", "10", "alice", "20.5", "bob"})))
	assert.EqualValues(t, 2, mustDecode(t, cmdZCARD([]string{"z"})))

	assert.EqualValues(t, "10.000000", mustDecode(t, cmdZSCORE([]string{"z", "alice"})))
	assert.EqualValues(t, "20.500000", mustDecode(t, cmdZSCORE([]string{"z", "bob"})))

	// A member that was never added has no score, and must not be given one.
	assert.Equal(t, constant.RespNil, cmdZSCORE([]string{"z", "nobody"}),
		"an absent member must be nil, not a score of zero")
	assert.Equal(t, constant.RespNil, cmdZSCORE([]string{"nosuchkey", "alice"}))
}

func TestCmdZScoreAfterZRem(t *testing.T) {
	ResetStores()
	assert.EqualValues(t, 1, mustDecode(t, cmdZADD([]string{"z", "1", "gone"})))
	assert.EqualValues(t, "1.000000", mustDecode(t, cmdZSCORE([]string{"z", "gone"})))

	assert.EqualValues(t, 1, mustDecode(t, cmdZREM([]string{"z", "gone"})))
	assert.Equal(t, constant.RespNil, cmdZSCORE([]string{"z", "gone"}),
		"a removed member must stop having a score")
}
