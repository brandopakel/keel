package core

import (
	"time"

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
//
// It samples e's stores, from where its last cycle left off, and counts what
// it takes against e.
func (e *Engine) ExpireCycle() int {
	samples := e.settings.expireSamples
	if e.replicaOf() != "" || samples <= 0 {
		return 0
	}
	if e.KeysWithExpiry() == 0 {
		// Redis zeroes a database's avg_ttl when nothing in it expires, and
		// its cycle, sampling nothing, folds a sample with nothing stale into
		// expired_stale_perc.
		e.avgTTL = 0
		e.expireStalePerc *= 1 - staleSampleWeight
		return 0
	}

	start := nanotime()
	e.aofBegin("")
	defer e.aofEnd()
	// What active expiry noted outside a cycle is not this cycle's sample.
	e.space.TakeTTLSamples()
	total, sampled := 0, 0
	// The time left to the keys found not yet due, over the rounds since
	// avg_ttl last took them, and how many of those rounds found any.
	var ttlSum, ttlSamples uint64
	updates := 0
	for round := 0; round < e.settings.expireRounds; round++ {
		examined, expired := 0, 0
		e.expireCursor = e.space.EachKeyspaceFrom(e.expireCursor, func(ks data_structure.Keyspace) {
			if examined < samples {
				n, d := ks.ActiveExpire(samples - examined)
				examined += n
				expired += d
			}
		})
		total += expired
		sampled += examined
		if sum, n := e.space.TakeTTLSamples(); n > 0 {
			ttlSum, ttlSamples = ttlSum+sum, ttlSamples+n
			updates++
		}
		// Another round only while the sample keeps coming up expired. The
		// threshold is what stops this being a full scan on a keyspace where
		// nothing has fallen due, and what makes it one on a keyspace where
		// everything has.
		done := examined == 0 || expired*100 < examined*e.settings.expirePercent
		// A cycle that would go on, but has drawn all the rounds it may, stops
		// at its cap: Redis's expired_time_cap_reached_count, whose cap is
		// time where Keel's is rounds.
		capped := !done && round == e.settings.expireRounds-1
		if capped {
			e.expireCapReached++
		}
		last := done || capped
		// Redis folds its sample into avg_ttl every 16 rounds and as it stops.
		if (round+1)%16 == 0 || last {
			e.foldAvgTTL(ttlSum, ttlSamples, updates)
			ttlSum, ttlSamples, updates = 0, 0, 0
		}
		if last {
			break
		}
	}

	if total > 0 {
		e.expiredKeys += uint64(total)
	}
	// Redis's estimate of the keys with a TTL that have fallen due and are
	// not yet removed: what this cycle found expired of what it sampled,
	// folded into a running average (expire.c, activeExpireCycle).
	stale := 0.0
	if sampled > 0 {
		stale = float64(total) / float64(sampled)
	}
	e.expireStalePerc = stale*staleSampleWeight + e.expireStalePerc*(1-staleSampleWeight)
	e.noteExpireCycle(start, nanotime())
	return total
}

// staleSampleWeight is the weight Redis's activeExpireCycle gives a cycle's
// sample in expired_stale_perc's running average, the old estimate keeping
// the rest.
const staleSampleWeight = 0.05

// avgTTLFactor is Redis's avg_ttl_factor (expire.c): 0.98 to the power of
// 1 to 16.
var avgTTLFactor = [16]float64{0.98, 0.9604, 0.941192, 0.922368, 0.903921, 0.885842, 0.868126, 0.850763,
	0.833748, 0.817073, 0.800731, 0.784717, 0.769022, 0.753642, 0.738569, 0.723798}

// foldAvgTTL folds into avg_ttl the mean of samples TTLs summing sum, taken
// over updates rounds, as Redis's activeExpireCycle folds its sample: the
// first estimate as it is, and after it the old one with a weight of 98% for
// each round, the new mean for the rest.
func (e *Engine) foldAvgTTL(sum, samples uint64, updates int) {
	if samples == 0 {
		return
	}
	mean := int64(sum / samples)
	if e.avgTTL == 0 {
		e.avgTTL = mean
		return
	}
	e.avgTTL = int64(float64(mean) + float64(e.avgTTL-mean)*avgTTLFactor[updates-1])
}

// ExpiredKeys reports how many of e's keys have been removed because their
// TTL had passed, by a read that found them so or by the cycle: INFO's
// expired_keys.
func (e *Engine) ExpiredKeys() uint64 { return e.space.Expired() }

// ActiveExpiredKeys reports how many keys e's cycle has removed: INFO's
// expired_keys_active.
func (e *Engine) ActiveExpiredKeys() uint64 { return e.expiredKeys }

// KeysWithExpiry reports how many of e's keys carry a TTL.
func (e *Engine) KeysWithExpiry() int {
	n := 0
	e.space.EachKeyspace(func(ks data_structure.Keyspace) { n += ks.KeysWithExpiry() })
	return n
}

// MaintainMemory advances physical table compaction without deleting logical
// keys or emitting persistence records. It is safe between event-loop phases
// on both primaries and replicas, including while an immutable append is pending.
//
// It compacts e's stores, from where its last pass left off.
func (e *Engine) MaintainMemory() int {
	work := 0
	deadline := time.Now().Add(time.Millisecond)
	// Rotate the first table family so continuous churn in one cannot starve
	// another. TTL, key lookup and collection indexes share the same budget.
	e.memoryFirstPhase = (e.memoryFirstPhase + 1) % 3
	e.memoryCursor = e.space.VisitKeyspacesFrom(e.memoryCursor, func(ks data_structure.Keyspace) bool {
		if work >= data_structure.ScanMaxWork || time.Now().After(deadline) {
			return false
		}
		for phase := 0; phase < 3; phase++ {
			if work >= data_structure.ScanMaxWork || time.Now().After(deadline) {
				return false
			}
			switch (phase + e.memoryFirstPhase) % 3 {
			case 0:
				if compact, ok := ks.(interface{ CompactLookup(int) int }); ok {
					work += compact.CompactLookup(data_structure.ScanMaxWork - work)
				}
			case 1:
				if compact, ok := ks.(interface{ CompactExpiry(int) int }); ok {
					work += compact.CompactExpiry(data_structure.ScanMaxWork - work)
				}
			case 2:
				// Other value families have no collection compactor yet. Avoid
				// scanning their keys merely to discover that at every value.
				switch values := ks.(type) {
				case *data_structure.Keyed[*data_structure.Set]:
					work += values.CompactValues(data_structure.ScanMaxWork - work)
				case *data_structure.Keyed[*data_structure.ZSet]:
					work += values.CompactValues(data_structure.ScanMaxWork - work)
				}
			}
		}
		return work < data_structure.ScanMaxWork && time.Now().Before(deadline)
	})
	return work
}
