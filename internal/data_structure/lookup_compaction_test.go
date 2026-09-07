package data_structure

import (
	"github.com/stretchr/testify/require"
	"hash/maphash"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestLookupCompactionRecoversPartlyOccupiedTable(t *testing.T) {
	for _, kind := range []string{"string", "collection"} {
		t.Run(kind, func(t *testing.T) {
			ResetKeyspaces()
			var ks Keyspace
			var put func(string)
			if kind == "string" {
				d := CreateDict()
				ks = d
				put = func(k string) { d.Put(k, d.NewObj("v")) }
			} else {
				d := NewKeyed[retentionValue]("retention")
				ks = d
				put = func(k string) { d.Put(k, 1) }
			}
			for i := 0; i < 100000; i++ {
				put(strconv.Itoa(i))
			}
			for i := 1000; i < 100000; i++ {
				ks.Delete(strconv.Itoa(i))
			}
			retained := heapBytes()
			if compact, ok := ks.(interface{ CompactLookup(int) int }); ok {
				for i := 0; i < 10000; i++ {
					require.LessOrEqual(t, compact.CompactLookup(37), 37)
				}
			}
			after := heapBytes()
			t.Logf("retained=%d after=%d recovered=%d", retained, after, int64(retained)-int64(after))
			require.Greater(t, int64(retained)-int64(after), int64(1<<20))
			require.Equal(t, 1000, ks.Len())
			for i := 0; i < 1000; i++ {
				require.True(t, ks.Has(strconv.Itoa(i)))
			}
			runtime.KeepAlive(ks)
		})
	}
}

func TestLookupCompactionMirrorsMutationsAndPreservesSlots(t *testing.T) {
	var m keyMap[int]
	m.hashOverride = func(key string) uint64 {
		if strings.HasPrefix(key, "collision:") {
			return 1
		}
		return maphash.String(keyLookupSeed, key)
	}
	expected := make(map[string]int)
	for i := 0; i < 8192; i++ {
		m.set(strconv.Itoa(i), i)
	}
	for i := 1024; i < 8192; i++ {
		m.del(strconv.Itoa(i))
	}
	for i := 0; i < 1024; i++ {
		expected[strconv.Itoa(i)] = i
	}
	for _, key := range []string{"collision:head", "collision:middle", "collision:tail"} {
		m.set(key, 42)
		expected[key] = 42
	}
	positions := make(map[string]uint64)
	for key := range expected {
		pos, _, _ := m.position(key)
		positions[key] = pos
	}
	require.Equal(t, 1, m.compact(1))
	require.NotNil(t, m.compaction)
	// Behind the cursor, slot reuse, entirely new hashes and collision-head
	// promotion must all be represented in the replacement table.
	m.del("0")
	delete(expected, "0")
	m.set("reused", 900)
	expected["reused"] = 900
	m.set("1", 901)
	expected["1"] = 901
	m.del("collision:head")
	delete(expected, "collision:head")
	m.del("collision:middle")
	delete(expected, "collision:middle")
	m.set("collision:new", 902)
	expected["collision:new"] = 902
	for i := 0; i < 1000; i++ {
		if i%7 == 0 {
			key := "new:" + strconv.Itoa(i)
			m.set(key, i)
			expected[key] = i
		}
		if i%11 == 0 {
			key := strconv.Itoa(i)
			m.del(key)
			delete(expected, key)
		}
		require.LessOrEqual(t, m.compact(13), 13)
	}
	require.Nil(t, m.compaction)
	require.Equal(t, len(expected), m.len())
	for key, value := range expected {
		got, ok := m.get(key)
		require.True(t, ok, key)
		require.Equal(t, value, got, key)
		if before, ok := positions[key]; ok {
			after, _, _ := m.position(key)
			require.Equal(t, before, after, "surviving key slot must not move")
		}
	}
	seen := make(map[string]int)
	var cursor uint64
	for {
		keys, work, next := m.scan(cursor, 17, nil, nil)
		require.LessOrEqual(t, work, 17)
		for _, key := range keys {
			seen[key]++
		}
		cursor = next
		if next == 0 {
			break
		}
	}
	require.Len(t, seen, len(expected))
	for key := range expected {
		require.Equal(t, 1, seen[key])
	}
}

func TestLookupCompactionStartBoundAndRegrowthCancellation(t *testing.T) {
	var m keyMap[int]
	for i := 0; i < 100000; i++ {
		m.set(strconv.Itoa(i), i)
	}
	for i := 1000; i < 100000; i++ {
		m.del(strconv.Itoa(i))
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	work := m.compact(37)
	runtime.ReadMemStats(&after)
	require.LessOrEqual(t, work, 37)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(256<<10), "start must not allocate for the original dataset size")
	require.NotNil(t, m.compaction)
	for i := 1000; i < 51000; i++ {
		m.set(strconv.Itoa(i), i)
	}
	require.Zero(t, m.compact(37))
	require.Nil(t, m.compaction)
	for i := 1000; i < 51000; i++ {
		m.del(strconv.Itoa(i))
	}
	require.Greater(t, m.compact(1), 0)
	for i := 0; i < 1000; i++ {
		m.del(strconv.Itoa(i))
	}
	require.Nil(t, m.compaction)
	require.Nil(t, m.lookup)
	require.Zero(t, m.lookupPeak)
}
