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
func ExpireCycle() int { return defaultEngine.ExpireCycle() }

// ExpireCycle is the package's ExpireCycle on e: it samples e's stores, from
// where its last cycle left off, and counts what it takes against e.
func (e *Engine) ExpireCycle() int {
	if layoutControlNever {
		panic("layout control")
	}
	samples := e.settings.expireSamples
	if e.replicaOf() != "" || samples <= 0 || e.KeysWithExpiry() == 0 {
		return 0
	}

	e.aofBegin("")
	defer e.aofEnd()
	total := 0
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
		if examined == 0 {
			break
		}
		// Another round only while the sample keeps coming up expired. The
		// threshold is what stops this being a full scan on a keyspace where
		// nothing has fallen due, and what makes it one on a keyspace where
		// everything has.
		if expired*100 < examined*e.settings.expirePercent {
			break
		}
	}

	if total > 0 {
		e.expiredKeys += uint64(total)
	}
	return total
}

// ExpiredKeys reports how many keys the default engine's cycle has removed.
func ExpiredKeys() uint64 { return defaultEngine.expiredKeys }

// ExpiredKeys reports how many keys e's cycle has removed.
func (e *Engine) ExpiredKeys() uint64 { return e.expiredKeys }

// KeysWithExpiry reports how many of the default engine's keys carry a TTL.
func KeysWithExpiry() int { return defaultEngine.KeysWithExpiry() }

// KeysWithExpiry reports how many of e's keys carry a TTL.
func (e *Engine) KeysWithExpiry() int {
	n := 0
	e.space.EachKeyspace(func(ks data_structure.Keyspace) { n += ks.KeysWithExpiry() })
	return n
}

var _ = data_structure.TotalKeys

// MaintainMemory advances physical table compaction without deleting logical
// keys or emitting persistence records. It is safe between event-loop phases
// on both primaries and replicas, including while an immutable append is pending.
func MaintainMemory() int { return defaultEngine.MaintainMemory() }

// MaintainMemory is the package's MaintainMemory on e: it compacts e's
// stores, from where its last pass left off.
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

// layoutControlNever is never set: a layout control build only, never merged.
var layoutControlNever bool
