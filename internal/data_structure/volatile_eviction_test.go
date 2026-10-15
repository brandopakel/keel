package data_structure

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// farFuture is an expiry instant no test reaches, in Unix milliseconds.
const farFuture = uint64(4_000_000_000_000)

// TestVolatilePoliciesEvictOnlyKeysWithATTL: each volatile policy chooses
// among the keys that carry a TTL, in every keyspace, and leaves the others,
// as Redis's samples its databases' expires rather than their keys.
func TestVolatilePoliciesEvictOnlyKeysWithATTL(t *testing.T) {
	t.Parallel()
	for _, policy := range []EvictionPolicy{EvictVolatileLRU, EvictVolatileLFU, EvictVolatileRandom, EvictVolatileTTL} {
		space := NewSpace(evictionLimits(policy, 5, 20))
		d := CreateDict(space)
		hashes := NewKeyed[*Hash](space, "hash")
		space.RegisterKeyspace(d)
		space.RegisterKeyspace(hashes)
		for i := 0; i < 10; i++ {
			d.Put("plain"+strconv.Itoa(i), d.NewObj("v"))
		}
		for i := 0; i < 30; i++ {
			// Each key is put, which may evict, before its TTL is set, so the
			// key just written is never a candidate for its own room.
			if i%2 == 0 {
				d.Put("ttl"+strconv.Itoa(i), d.NewObj("v"))
				d.SetExpiryAt("ttl"+strconv.Itoa(i), farFuture+uint64(i))
			} else {
				hashes.Put("ttl"+strconv.Itoa(i), NewHash())
				hashes.SetExpiryAt("ttl"+strconv.Itoa(i), farFuture+uint64(i))
			}
		}
		for i := 0; i < 10; i++ {
			assert.True(t, d.Has("plain"+strconv.Itoa(i)), "policy %d evicted a key with no TTL", policy)
		}
		assert.Equal(t, 20, space.TotalKeys(), "policy %d", policy)
		assert.EqualValues(t, 20, space.Evicted(), "policy %d took one key with a TTL for each one put over the bound", policy)
		assert.False(t, space.OverLimit())
	}
}

// TestAVolatilePolicyWithNoKeyWithATTLLeavesTheSpaceOver: with no key that
// carries a TTL there is nothing a volatile policy may evict, and noeviction
// evicts nothing at all, so the space stays over its bound for the engine to
// refuse what would grow it.
func TestAVolatilePolicyWithNoKeyWithATTLLeavesTheSpaceOver(t *testing.T) {
	t.Parallel()
	for _, policy := range []EvictionPolicy{EvictVolatileLRU, EvictVolatileLFU, EvictVolatileRandom, EvictVolatileTTL, EvictNone} {
		d := newTestDict(evictionLimits(policy, 5, 2))
		for i := 0; i < 3; i++ {
			d.Put("k"+strconv.Itoa(i), d.NewObj("v"))
		}
		assert.Equal(t, 3, d.Len(), "policy %d", policy)
		assert.Zero(t, d.space.Evicted(), "policy %d", policy)
		assert.True(t, d.space.OverLimit(), "policy %d", policy)
	}
	d := newTestDict(evictionLimits(EvictNone, 5, 2))
	for i := 0; i < 3; i++ {
		d.Put("k"+strconv.Itoa(i), d.NewObj("v"))
		d.SetExpiryAt("k"+strconv.Itoa(i), farFuture)
	}
	assert.Equal(t, 3, d.Len(), "noeviction takes no key, TTL or not")
}

// TestVolatileTTLEvictsTheKeyDueSoonest: volatile-ttl takes, of its sample of
// the keys that carry a TTL, the one that falls due first, as Redis ranks them
// (evict.c, evictionPoolPopulate).
func TestVolatileTTLEvictsTheKeyDueSoonest(t *testing.T) {
	t.Parallel()
	d := newTestDict(evictionLimits(EvictVolatileTTL, 10, 3))
	for key, at := range map[string]uint64{"late": farFuture + 3000, "soon": farFuture + 1000, "middle": farFuture + 2000} {
		d.Put(key, d.NewObj("v"))
		d.SetExpiryAt(key, at)
	}
	for _, gone := range []string{"soon", "middle", "late"} {
		d.Put("plain-after-"+gone, d.NewObj("v"))
		assert.False(t, d.Has(gone), "%s fell due soonest of what was left", gone)
	}
	d.Put("one-too-many", d.NewObj("v"))
	assert.Equal(t, 4, d.Len(), "with no key that carries a TTL left, nothing is evicted")
}

// TestVolatileLRUAndLFURankByUseAsTheirNamesakes: among the keys that carry a
// TTL, volatile-lru keeps the one read most recently, and volatile-lfu the one
// read most often.
func TestVolatileLRUAndLFURankByUseAsTheirNamesakes(t *testing.T) {
	t.Parallel()
	for _, policy := range []EvictionPolicy{EvictVolatileLRU, EvictVolatileLFU} {
		d := newTestDict(evictionLimits(policy, 10, 3))
		for _, key := range []string{"used", "unused"} {
			d.Put(key, d.NewObj("v"))
			d.SetExpiryAt(key, farFuture)
		}
		d.Put("plain", d.NewObj("v"))
		for i := 0; i < 20; i++ {
			d.Get("used")
		}
		d.Put("more", d.NewObj("v"))
		assert.True(t, d.Has("used"), "policy %d", policy)
		assert.False(t, d.Has("unused"), "policy %d", policy)
		assert.True(t, d.Has("plain"), "policy %d", policy)
	}
}

// TestChangingThePolicyDropsThePool: candidates pooled under one policy are
// not evicted under another that would not have chosen them, such as a key
// with no TTL once the policy is volatile.
func TestChangingThePolicyDropsThePool(t *testing.T) {
	t.Parallel()
	d := newTestDict(evictionLimits(EvictLRU, 5, 4))
	for i := 0; i < 6; i++ {
		d.Put("plain"+strconv.Itoa(i), d.NewObj("v"))
	}
	require.NotEmpty(t, d.space.pool, "allkeys-lru pooled candidates")
	limits := d.space.Limits()
	limits.Eviction = EvictVolatileLRU
	d.space.SetLimits(limits)
	assert.Empty(t, d.space.pool)
	before := d.Len()
	d.Put("one-more", d.NewObj("v"))
	assert.Equal(t, before+1, d.Len(), "no key carries a TTL, so volatile-lru takes none")
}

// TestMayStayOverIsTheVolatilePoliciesAndNoeviction: only a policy that may
// find nothing to evict can leave a space over its bound, which is what the
// engine tests before it looks at memory at all.
func TestMayStayOverIsTheVolatilePoliciesAndNoeviction(t *testing.T) {
	t.Parallel()
	for policy, may := range map[EvictionPolicy]bool{EvictLRU: false, EvictLFU: false, EvictRandom: false,
		EvictVolatileLRU: true, EvictVolatileLFU: true, EvictVolatileRandom: true, EvictVolatileTTL: true, EvictNone: true} {
		assert.Equal(t, may, policy.MayStayOver(), "policy %d", policy)
	}
}
