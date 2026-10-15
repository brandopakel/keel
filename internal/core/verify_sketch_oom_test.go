package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSketchWritesAreRefusedForMemoryAsRedisBloomMarksThem: the RedisBloom
// commands this family adds carry the deny-oom mode RedisBloom registers them
// with (rebloom.c, rm_cms.c), so under noeviction, once the keyspace is over
// its bound, BF.INSERT, CF.INSERT, CF.INSERTNX and CMS.MERGE are refused with
// Redis's OOM error, as Redis refuses a deny-oom module command; CF.COMPACT,
// registered without it, and the reads run.
func TestSketchWritesAreRefusedForMemoryAsRedisBloomMarksThem(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{MaxMemory: 1, Eviction: EvictNone})
	s := newSession(t, e)
	// The filters are put in place directly: any write through a command
	// would itself cross the bound of one byte.
	seedCuckoo(e, "cf")
	seedBloom(e, "bf")
	require.True(t, e.space.OverLimit())
	for _, cmd := range [][]string{
		{"BF.INSERT", "bf", "ITEMS", "a"},
		{"CF.INSERT", "cf", "ITEMS", "a"},
		{"CF.INSERTNX", "cf", "ITEMS", "a"},
		{"CMS.MERGE", "cms", "1", "cms2"},
	} {
		assert.Equal(t, oomReply, s.send(cmd...), "%v", cmd)
	}
	assert.Equal(t, "+OK\r\n", s.send("CF.COMPACT", "cf"), "CF.COMPACT is not deny-oom")
	assert.Equal(t, ":1\r\n", s.send("BF.CARD", "bf"))
	assert.Equal(t, ":1\r\n", s.send("CF.COUNT", "cf", "a"))
}
