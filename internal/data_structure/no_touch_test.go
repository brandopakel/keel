package data_structure

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNoTouchLeavesAccessAloneUnderEveryPolicy: while a CLIENT NO-TOUCH
// connection's command runs, a read leaves the key's access word and the
// space's clock as they were, whichever maxmemory policy the space has,
// volatile and noeviction included, as Redis's lookupKey skips updating the
// LRU or LFU field under LOOKUP_NOTOUCH whatever the policy (db.c). It does
// not skip expiry: a read of a key whose TTL has passed still removes it and
// counts it in expired_keys. Once no-touch is off, the same read records the
// access again.
func TestNoTouchLeavesAccessAloneUnderEveryPolicy(t *testing.T) {
	t.Parallel()
	policies := map[string]EvictionPolicy{
		"allkeys-lru": EvictLRU, "allkeys-lfu": EvictLFU, "allkeys-random": EvictRandom,
		"volatile-lru": EvictVolatileLRU, "volatile-lfu": EvictVolatileLFU,
		"volatile-random": EvictVolatileRandom, "volatile-ttl": EvictVolatileTTL, "noeviction": EvictNone,
	}
	for name, policy := range policies {
		space := NewSpace(evictionLimits(policy, 5, 100))
		d := CreateDict(space)
		hashes := NewKeyed[*Hash](space, "hash")
		space.RegisterKeyspace(d)
		space.RegisterKeyspace(hashes)
		d.Put("k", d.NewObj("v"))
		d.SetExpiryAt("k", farFuture)
		hashes.Put("h", NewHash())
		hashes.SetExpiryAt("h", farFuture)
		d.Put("gone", d.NewObj("v"))
		d.SetExpiryAt("gone", 1)
		hashes.Put("gone", NewHash())
		hashes.SetExpiryAt("gone", 1)
		// Reads move the clock on, so the words below are not the clock's.
		d.Get("k")
		hashes.Get("h")

		access := func() (uint64, uint64) {
			obj, ok := d.dictStore.getPtr("k")
			require.True(t, ok, name)
			e, ok := hashes.items.getPtr("h")
			require.True(t, ok, name)
			return obj.Access, e.access
		}
		dictWord, hashWord := access()
		clock := space.NewAccess()

		space.SetNoTouch(true)
		require.NotNil(t, d.Get("k"), name)
		_, ok := hashes.Get("h")
		require.True(t, ok, name)
		gotDict, gotHash := access()
		require.Equal(t, dictWord, gotDict, "%s: a string read under no-touch", name)
		require.Equal(t, hashWord, gotHash, "%s: a hash read under no-touch", name)
		require.Equal(t, clock, space.NewAccess(), "%s: the clock did not move", name)
		require.Nil(t, d.Get("gone"), name)
		_, ok = hashes.Get("gone")
		require.False(t, ok, name)
		require.False(t, d.Has("gone") || hashes.Exists("gone"), "%s: a read under no-touch still reaps", name)
		require.EqualValues(t, 2, space.Expired(), "%s: and counts what it reaps", name)

		space.SetNoTouch(false)
		d.Get("k")
		hashes.Get("h")
		gotDict, gotHash = access()
		require.NotEqual(t, dictWord, gotDict, "%s: a string read records the access", name)
		require.NotEqual(t, hashWord, gotHash, "%s: a hash read records the access", name)
	}
}
