package data_structure

import "time"

// Active expiry.
//
// A key with a TTL used to go away only when something next looked at it. That
// is cheap and it is enough for correctness - nothing ever reads an expired
// value - but it is not enough for memory. A key written once with an hour's
// TTL and never read again holds its bytes until eviction gets round to it,
// which under no memory pressure is never. Five hundred keys given a one-second
// TTL and then left alone still counted in DBSIZE and used_memory a minute
// later, which is how this was noticed.
//
// The fix cannot be to walk every key with a TTL: that is O(n) at intervals for
// keys that may all be perfectly alive. Redis samples instead, and repeats
// while the sample keeps coming up expired - the same shape as the eviction
// sampling next door, and for the same reason. If a quarter of twenty random
// keys have fallen due then a lot of the keyspace has, and another round is
// worth it; if none has, there is nothing to find and the cycle costs twenty
// map reads.
//
// The sample is drawn from the expiry table rather than the keyspace, so it
// only ever looks at keys that have a TTL at all. A server whose keys never
// expire pays one length check.

// ActiveExpire examines up to samples keys with a TTL and removes those that
// have fallen due, reporting how many of each.
//
// Removals go through noteRemoval, because a key reaped here has no command
// behind it and an append-only log that missed it would replay the key back
// into existence with an expiry already in the past.
func (d *Dict) ActiveExpire(samples int) (examined, expired int) {
	if len(d.expiredDictStore) == 0 || samples <= 0 {
		return 0, 0
	}
	now := uint64(time.Now().UnixMilli())

	// Collected before deleting rather than deleted while ranging. Go permits
	// deleting during a range, but the sample is meant to be of the table as it
	// was, and a range that is also modifying the map is a harder thing to
	// reason about than one extra slice of a few names.
	var doomed []string
	for key, at := range d.expiredDictStore {
		examined++
		if at <= now {
			doomed = append(doomed, key)
		}
		if examined >= samples {
			break
		}
	}

	for _, key := range doomed {
		if d.Del(key) {
			expired++
			noteRemoval(d, key)
		}
	}
	return examined, expired
}

// KeysWithExpiry is how many keys carry a TTL, for INFO and for a cycle that
// wants to know whether there is anything to do.
func (d *Dict) KeysWithExpiry() int { return len(d.expiredDictStore) }
