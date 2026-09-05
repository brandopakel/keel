package core

import (
	"github.com/brandopakel/keel/internal/config"
	"github.com/brandopakel/keel/internal/data_structure"
)

// ExpireCycle removes keys whose TTL has passed, without waiting for anyone to
// read them, and reports how many it took.
//
// Called once per turn of the event loop. Most turns it costs one length check,
// because most servers have no expired keys waiting at any given instant. When
// there are, it samples and keeps sampling while the sample says there are more
// - the same argument the eviction pool makes, that a random sample is a cheap
// way to tell a keyspace with a problem from one without.
func ExpireCycle() int {
	if config.ReplicaOf != "" || config.ActiveExpireSamples <= 0 || KeysWithExpiry() == 0 {
		return 0
	}

	total := 0
	for round := 0; round < config.ActiveExpireRounds; round++ {
		examined, expired := 0, 0
		expireCursor = data_structure.EachKeyspaceFrom(expireCursor, func(ks data_structure.Keyspace) {
			if examined < config.ActiveExpireSamples {
				n, d := ks.ActiveExpire(config.ActiveExpireSamples - examined)
				examined += n
				expired += d
			}
		})
		total += expired
		if examined == 0 {
			break
		}
		// Another round only while the sample keeps coming up expired. The
		// threshold is what stops this being a full scan on a keyspace where
		// nothing has fallen due, and what makes it one on a keyspace where
		// everything has.
		if expired*100 < examined*config.ActiveExpirePercent {
			break
		}
	}

	if total > 0 {
		// The removals were recorded by the OnRemove hook while the cycle ran,
		// and nothing else is going to commit them: this happens between
		// commands, not inside one, so no aofCommit is coming.
		aofCommitExtras()
		expiredKeys += uint64(total)
	}
	return total
}

// expiredKeys counts what active expiry has reclaimed, for INFO. Keys reaped
// lazily by a read are not counted here, because the number is here to answer
// whether the cycle is keeping up.
var expiredKeys uint64
var expireCursor int

// ExpiredKeys reports how many keys the cycle has removed.
func ExpiredKeys() uint64 { return expiredKeys }

// KeysWithExpiry reports how many keys are carrying a TTL.
func KeysWithExpiry() int {
	n := 0
	data_structure.EachKeyspace(func(ks data_structure.Keyspace) { n += ks.KeysWithExpiry() })
	return n
}

var _ = data_structure.TotalKeys
