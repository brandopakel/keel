package data_structure

import (
	"github.com/brandopakel/keel/internal/config"
	"github.com/stretchr/testify/require"
	"runtime"
	"strconv"
	"testing"
)

func TestPartlyOccupiedExpiryTableReleasesRetainedHeap(t *testing.T) {
	for _, kind := range []string{"string", "collection"} {
		t.Run(kind, func(t *testing.T) {
			withEviction(t, config.EvictFirst, 5, 1000000)
			ResetKeyspaces()
			var ks Keyspace
			var put func(string)
			if kind == "string" {
				d := CreateDict()
				ks = d
				put = func(key string) { d.Put(key, d.NewObj("value")) }
			} else {
				k := NewKeyed[retentionValue]("retention")
				ks = k
				put = func(key string) { k.Put(key, 1) }
			}
			RegisterKeyspace(ks)
			keys := make([]string, 100000)
			for i := range keys {
				keys[i] = "ttl:" + strconv.Itoa(i)
				put(keys[i])
			}
			for _, key := range keys[:1000] {
				ks.SetExpiryAt(key, 1<<60)
			}
			baseline := heapBytes()
			for _, key := range keys {
				ks.SetExpiryAt(key, 1<<60)
			}
			for _, key := range keys[1000:] {
				ks.ClearExpiry(key)
			}
			retained := heapBytes()
			// Drive the same bounded maintenance hook that the event loop schedules.
			if compact, ok := ks.(interface{ CompactExpiry(int) int }); ok {
				for i := 0; i < 10000; i++ {
					require.LessOrEqual(t, compact.CompactExpiry(37), 37)
				}
			}
			after := heapBytes()
			t.Logf("baseline=%d retained=%d after=%d recovered=%d", baseline, retained, after, int64(retained)-int64(after))
			require.Less(t, after, baseline+512<<10)
			require.Equal(t, 1000, ks.KeysWithExpiry())
			for _, key := range keys[:1000] {
				at, ok := ks.GetExpiry(key)
				require.True(t, ok)
				require.EqualValues(t, 1<<60, at)
			}
		})
	}
}

func TestExpiryCompactionMirrorsMutationsAcrossCursor(t *testing.T) {
	for _, kind := range []string{"string", "collection"} {
		t.Run(kind, func(t *testing.T) {
			withEviction(t, config.EvictFirst, 5, 1000000)
			ResetKeyspaces()
			var ks Keyspace
			var put func(string)
			if kind == "string" {
				d := CreateDict()
				ks = d
				put = func(key string) { d.Put(key, d.NewObj("v")) }
			} else {
				k := NewKeyed[retentionValue]("ttl")
				ks = k
				put = func(key string) { k.Put(key, 1) }
			}
			RegisterKeyspace(ks)
			expected := map[string]uint64{}
			for i := 0; i < 5120; i++ {
				key := strconv.Itoa(i)
				put(key)
				ks.SetExpiryAt(key, 1<<60)
				if i >= 4000 {
					expected[key] = 1 << 60
				}
			}
			for i := 0; i < 4000; i++ {
				ks.ClearExpiry(strconv.Itoa(i))
			}
			compact := ks.(interface{ CompactExpiry(int) int })
			require.Equal(t, 1, compact.CompactExpiry(1))
			// Insert/replace behind the cursor, remove unvisited keys, reuse a freed
			// slot and append keys beyond the captured end. Final TTLs must win.
			ks.SetExpiryAt("0", 1<<60+1)
			expected["0"] = 1<<60 + 1
			ks.SetExpiryAt("4001", 1<<60+2)
			expected["4001"] = 1<<60 + 2
			require.True(t, ks.Delete("5119"))
			delete(expected, "5119")
			require.True(t, ks.ClearExpiry("5000"))
			delete(expected, "5000")
			put("4999")
			delete(expected, "4999")
			for i := 0; i < 20; i++ {
				key := "new:" + strconv.Itoa(i)
				put(key)
				ks.SetExpiryAt(key, 1<<60+3)
				expected[key] = 1<<60 + 3
			}
			for i := 0; i < 300; i++ {
				require.LessOrEqual(t, compact.CompactExpiry(37), 37)
			}
			require.Equal(t, len(expected), ks.KeysWithExpiry())
			for key, want := range expected {
				at, ok := ks.GetExpiry(key)
				require.True(t, ok, key)
				require.Equal(t, want, at, key)
			}
			for _, key := range []string{"5119", "5000", "4999"} {
				_, ok := ks.GetExpiry(key)
				require.False(t, ok, key)
			}
		})
	}
}

func TestExpiryCompactionCancelsRegrowthAndEmptyTable(t *testing.T) {
	withEviction(t, config.EvictFirst, 5, 1000000)
	ResetKeyspaces()
	d := CreateDict()
	RegisterKeyspace(d)
	for i := 0; i < 4096; i++ {
		key := strconv.Itoa(i)
		d.Put(key, d.NewObj("v"))
		d.SetExpiryAt(key, 1<<60)
	}
	for i := 0; i < 3500; i++ {
		d.ClearExpiry(strconv.Itoa(i))
	}
	require.Equal(t, 1, d.CompactExpiry(1))
	require.NotNil(t, d.expiryCompaction)
	for i := 0; i < 3000; i++ {
		d.SetExpiryAt(strconv.Itoa(i), 1<<60)
	}
	require.Zero(t, d.CompactExpiry(100))
	require.Nil(t, d.expiryCompaction)
	require.Equal(t, 3596, d.KeysWithExpiry())
	for i := 0; i < 3000; i++ {
		d.ClearExpiry(strconv.Itoa(i))
	}
	d.CompactExpiry(1)
	require.NotNil(t, d.expiryCompaction)
	for i := 3500; i < 4096; i++ {
		d.ClearExpiry(strconv.Itoa(i))
	}
	require.Nil(t, d.expiryCompaction)
	require.Nil(t, d.expiredDictStore)
	require.Zero(t, d.CompactExpiry(100))
}

func TestExpiryCompactionStartDoesNotCopyTheTableOrAllKeyNames(t *testing.T) {
	withEviction(t, config.EvictFirst, 5, 1000000)
	ResetKeyspaces()
	d := CreateDict()
	RegisterKeyspace(d)
	for i := 0; i < 420000; i++ {
		key := strconv.Itoa(i)
		d.Put(key, d.NewObj("v"))
		d.SetExpiryAt(key, 1<<60)
	}
	for i := 0; i < 320000; i++ {
		d.ClearExpiry(strconv.Itoa(i))
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	work := d.CompactExpiry(37)
	runtime.ReadMemStats(&after)
	require.LessOrEqual(t, work, 37)
	require.NotNil(t, d.expiryCompaction)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(256<<10), "no whole-table copy or key-name enumeration at startup")
	require.Equal(t, 100000, d.KeysWithExpiry())
}
