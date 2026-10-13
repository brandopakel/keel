package data_structure

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRenameMovesTheValueAndItsExpiry: Rename moves a value, the same
// object, to a new name with its expiry, in a string keyspace and a keyed
// one, and charges the keyspace for the new name and nothing more; Touch
// reports whether a key is there and reaps one whose expiry has passed.
func TestRenameMovesTheValueAndItsExpiry(t *testing.T) {
	t.Parallel()
	space := NewSpace(evictionLimits(EvictRandom, 5, 1000000))
	d := CreateDict(space)
	k := NewKeyed[retentionValue](space, "retention")
	space.RegisterKeyspace(d)
	space.RegisterKeyspace(k)
	d.Put("from", d.NewObj("value"))
	k.Put("from", 7)
	for _, ks := range []Keyspace{d, k} {
		ks.SetExpiryAt("from", 1<<60)
		before := ks.MemUsed()
		require.True(t, ks.Rename("from", "tox"), ks.KeyspaceName())
		require.False(t, ks.Has("from"))
		require.True(t, ks.Has("tox"))
		at, expires := ks.GetExpiry("tox")
		require.True(t, expires)
		require.Equal(t, uint64(1<<60), at)
		_, stale := ks.GetExpiry("from")
		require.False(t, stale)
		require.Equal(t, before-1, ks.MemUsed(), "%s: the name is one byte shorter, and nothing else changed", ks.KeyspaceName())
		require.False(t, ks.Rename("missing", "other"))
		require.True(t, ks.Touch("tox"))
		require.False(t, ks.Touch("missing"))
		ks.SetExpiryAt("tox", 1)
		require.False(t, ks.Touch("tox"), "an expired key is not there")
		require.Zero(t, ks.Len())
	}
	require.Equal(t, uint64(0), d.MemUsed()+k.MemUsed())
	value, ok := k.Peek("tox")
	require.False(t, ok)
	require.Zero(t, value)
}
