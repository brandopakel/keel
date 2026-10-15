package data_structure

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExpiredCountsEveryKeyATTLRemoved: a key removed because its TTL had
// passed counts once, whatever found it so - a read, a look for the name or
// active expiry - as Redis's stat_expiredkeys counts it. A look that only
// reports, and an eviction, do not count.
func TestExpiredCountsEveryKeyATTLRemoved(t *testing.T) {
	t.Parallel()
	space := NewSpace(Limits{})
	dict := CreateDict(space)
	hashes := NewKeyed[*Hash](space, "hash")
	space.RegisterKeyspace(dict)
	space.RegisterKeyspace(hashes)
	for _, key := range []string{"read", "has", "peeked", "active"} {
		dict.Put(key, dict.NewObj("v"))
		dict.SetExpiryAt(key, 1)
	}
	dict.Put("alive", dict.NewObj("v"))
	dict.SetExpiryAt("alive", math.MaxUint64)
	for _, key := range []string{"get", "exists", "peeked", "active"} {
		hashes.Put(key, NewHash())
		hashes.SetExpiryAt(key, 1)
	}

	assert.Nil(t, dict.Get("read"))
	assert.False(t, dict.Has("has"))
	_, ok := hashes.Get("get")
	assert.False(t, ok)
	assert.False(t, hashes.Exists("exists"))
	assert.EqualValues(t, 4, space.Expired(), "each read or look that removed a key counts it")

	assert.Nil(t, dict.Peek("peeked"))
	_, ok = hashes.Peek("peeked")
	assert.False(t, ok)
	assert.EqualValues(t, 4, space.Expired(), "a look that reports removes nothing, and counts nothing")

	_, expired := dict.ActiveExpire(10)
	require.Equal(t, 2, expired, "peeked and active; alive lives")
	_, expired = hashes.ActiveExpire(10)
	require.Equal(t, 2, expired)
	assert.EqualValues(t, 8, space.Expired(), "active expiry counts what it removes")

	space.ResetExpired()
	assert.Zero(t, space.Expired())
}

// TestEvictionIsNotExpiry: a key evicted to make room is evicted_keys', not
// expired_keys', TTL or not.
func TestEvictionIsNotExpiry(t *testing.T) {
	t.Parallel()
	space := NewSpace(Limits{MaxKeys: 1})
	dict := CreateDict(space)
	space.RegisterKeyspace(dict)
	dict.Put("first", dict.NewObj("v"))
	dict.SetExpiryAt("first", math.MaxUint64)
	dict.Put("second", dict.NewObj("v"))
	require.EqualValues(t, 1, space.Evicted())
	assert.Zero(t, space.Expired())
}
