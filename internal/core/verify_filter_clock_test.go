package core

import (
	"testing"

	"github.com/brandopakel/keel/internal/data_structure"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// steppingClock is an expiry clock that reads before for its first n reads and
// after from then on: a TTL at after falls due between a command's n-th and
// its next read of the clock.
func steppingClock(n int, before, after uint64) func() uint64 {
	reads := 0
	return func() uint64 {
		reads++
		if reads <= n {
			return before
		}
		return after
	}
}

// TestFilterCommandsLookTheirFilterUpOnce: a filter command that goes on to
// use its filter looks it up once and keeps what it found. Were it to check
// the key's status and then look the filter up again, the space's clock is
// read twice, and a TTL falling due between the two reads leaves the command
// holding no filter (#218's review). Each operation runs here with the clock
// stepping past the filter's TTL right after its first read: it answers as
// for the filter it found then, and does not dereference a filter that is
// gone. With the earlier two lookups, each of these but CF.COUNT panics, and
// CF.COUNT answers 0 for an item the filter holds.
func TestFilterCommandsLookTheirFilterUpOnce(t *testing.T) {
	t.Parallel()
	const due = 1_000_000
	for _, tc := range []struct {
		name string
		seed func(e *Engine, key string)
		op   func(t *testing.T, e *Engine, key string) error
	}{
		{"CF.INSERT", seedCuckoo, func(t *testing.T, e *Engine, key string) error {
			return e.opCFInsert(key, cfInsertRequest{capacity: 100, items: []string{"b"}}, func(cfInsertResult) {})
		}},
		{"CF.INSERTNX", seedCuckoo, func(t *testing.T, e *Engine, key string) error {
			return e.opCFInsert(key, cfInsertRequest{capacity: 100, nx: true, items: []string{"a"}}, func(cfInsertResult) {})
		}},
		{"CF.COMPACT", seedCuckoo, func(t *testing.T, e *Engine, key string) error { return e.opCFCompact(key) }},
		{"CF.DEL", seedCuckoo, func(t *testing.T, e *Engine, key string) error { _, err := e.opCFDel(key, "a"); return err }},
		{"CF.INFO", seedCuckoo, func(t *testing.T, e *Engine, key string) error { _, err := e.opCFInfo(key); return err }},
		{"CF.COUNT", seedCuckoo, func(t *testing.T, e *Engine, key string) error {
			if n := e.opCFCount(key, "a"); n != 1 {
				t.Errorf("CF.COUNT answered %d for the item the filter holds", n)
			}
			return nil
		}},
		{"BF.INSERT", seedBloom, func(t *testing.T, e *Engine, key string) error {
			return e.opBFInsert(key, bfInsertRequest{r: bfReservation{capacity: 100, errorRate: 0.01, expansion: 2},
				items: []string{"b"}}, func(bool, error) {})
		}},
		{"BF.INFO", seedBloom, func(t *testing.T, e *Engine, key string) error { _, err := e.opBFInfo(key); return err }},
		{"BF.CARD", seedBloom, func(t *testing.T, e *Engine, key string) error {
			n, err := e.opBFCard(key)
			if err == nil && n != 1 {
				t.Errorf("BF.CARD answered %d for a filter of one item", n)
			}
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEngine(t, Options{})
			const key = "filter"
			tc.seed(e, key)
			ks, held := e.space.OwnerOf(key)
			require.True(t, held)
			setExpiry(t, ks, key, due)
			e.space.ExpiryClock = steppingClock(1, due-1, due)
			defer func() { e.space.ExpiryClock = nil }()
			require.NotPanics(t, func() { assert.NoError(t, tc.op(t, e, key)) })
		})
	}
}

func seedCuckoo(e *Engine, key string) {
	f := data_structure.CreateCuckooFilter(100)
	f.Insert("a")
	e.cfStore.Put(key, f)
}

func seedBloom(e *Engine, key string) {
	b := data_structure.CreateSBChain(100, 0.01, 2)
	b.Add("a")
	e.sbStore.Put(key, b)
}

// setExpiry gives key, held by ks, an expiry at the instant at.
func setExpiry(t *testing.T, ks data_structure.Keyspace, key string, at uint64) {
	t.Helper()
	switch store := ks.(type) {
	case *data_structure.Keyed[*data_structure.CuckooFilter]:
		store.SetExpiryAt(key, at)
	case *data_structure.Keyed[*data_structure.SBChain]:
		store.SetExpiryAt(key, at)
	default:
		t.Fatalf("no expiry setter for %T", ks)
	}
}

// TestAFilterReapedOnLookupCountsAsExpired: a filter whose TTL has passed is
// removed by the command that looks it up, and counted once in expired_keys,
// as Redis counts a key a read finds expired (#210): through Get for a
// command that uses the filter, and through the status check after Peek for
// INFO and CARD, which do not count a use. Each answers as for a key that
// holds nothing.
func TestAFilterReapedOnLookupCountsAsExpired(t *testing.T) {
	t.Parallel()
	const due = 1_000_000
	for _, tc := range []struct {
		name string
		seed func(e *Engine, key string)
		op   func(t *testing.T, e *Engine, key string)
	}{
		{"CF.COMPACT", seedCuckoo, func(t *testing.T, e *Engine, key string) {
			assert.Equal(t, errCFCompactNotFound, e.opCFCompact(key))
		}},
		{"CF.INFO", seedCuckoo, func(t *testing.T, e *Engine, key string) {
			_, err := e.opCFInfo(key)
			assert.Equal(t, errFilterNotFound, err)
		}},
		{"CF.COUNT", seedCuckoo, func(t *testing.T, e *Engine, key string) {
			assert.Zero(t, e.opCFCount(key, "a"))
		}},
		{"BF.INFO", seedBloom, func(t *testing.T, e *Engine, key string) {
			_, err := e.opBFInfo(key)
			assert.Equal(t, errFilterNotFound, err)
		}},
		{"BF.CARD", seedBloom, func(t *testing.T, e *Engine, key string) {
			n, err := e.opBFCard(key)
			assert.NoError(t, err)
			assert.Zero(t, n)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEngine(t, Options{})
			const key = "filter"
			tc.seed(e, key)
			ks, held := e.space.OwnerOf(key)
			require.True(t, held)
			setExpiry(t, ks, key, due)
			e.space.ExpiryClock = func() uint64 { return due }
			defer func() { e.space.ExpiryClock = nil }()
			before := e.ExpiredKeys()
			tc.op(t, e, key)
			assert.Equal(t, before+1, e.ExpiredKeys(), "the reap is counted once")
			e.space.ExpiryClock = nil
			_, held = e.space.OwnerOf(key)
			assert.False(t, held, "and the filter is gone")
		})
	}
}
