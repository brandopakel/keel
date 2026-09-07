package data_structure

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// drain walks a sharded map to completion the way a client would, and reports
// every key it was given along with how many calls it took.
func drain(t *testing.T, m *shardedMap[int], budget int) ([]string, int) {
	t.Helper()
	var seen []string
	cursor, calls := uint64(0), 0
	for {
		var next uint64
		seen, _, next = m.scan(cursor, budget, nil, seen)
		calls++
		require.Less(t, calls, 20000, "the walk must terminate")
		if next == 0 {
			return seen, calls
		}
		cursor = next
	}
}

func TestShardedScanReturnsEveryKeyExactlyOnce(t *testing.T) {
	for _, keys := range []int{0, 1, 7, 1000, 5000} {
		m := &shardedMap[int]{}
		for i := 0; i < keys; i++ {
			m.set("key:"+strconv.Itoa(i), i)
		}
		require.Equal(t, keys, m.len())

		seen, _ := drain(t, m, 10)
		assert.Len(t, seen, keys, "%d keys: every key is returned, and none twice", keys)

		unique := map[string]int{}
		for _, key := range seen {
			unique[key]++
		}
		assert.Len(t, unique, keys, "%d keys: no duplicates", keys)
		for i := 0; i < keys; i++ {
			assert.Equal(t, 1, unique["key:"+strconv.Itoa(i)], "key:%d appears once", i)
		}
	}
}

// The cursor is a shard index, so a small keyspace must not cost a call per
// shard: skipping an empty shard is a nil check, and the walk keeps going.
func TestShardedScanFinishesSmallKeyspacesQuickly(t *testing.T) {
	m := &shardedMap[int]{}
	for i := 0; i < 5; i++ {
		m.set("k"+strconv.Itoa(i), i)
	}
	seen, calls := drain(t, m, 10)
	assert.Len(t, seen, 5)
	assert.LessOrEqual(t, calls, 6, "five keys must not take a call per shard")
}

// budget bounds keys examined rather than keys returned, so a filter that
// rejects everything still terminates and still costs bounded work per call.
func TestShardedScanBudgetsExaminedKeysNotReturnedKeys(t *testing.T) {
	m := &shardedMap[int]{}
	for i := 0; i < 4000; i++ {
		m.set("key:"+strconv.Itoa(i), i)
	}

	none := func(string) bool { return false }
	cursor, calls, totalExamined, worst := uint64(0), 0, 0, 0
	var kept []string
	for {
		var examined int
		var next uint64
		kept, examined, next = m.scan(cursor, 100, none, kept)
		calls++
		totalExamined += examined
		if examined > worst {
			worst = examined
		}
		require.Less(t, calls, 20000, "the walk must terminate")
		if next == 0 {
			break
		}
		cursor = next
	}
	assert.Empty(t, kept, "the filter rejected every key")
	assert.Equal(t, 4000, totalExamined, "but every key was still examined")
	assert.Greater(t, calls, 1, "and it took more than one call to do it")
	assert.LessOrEqual(t, worst, 100, "COUNT is an actual work ceiling")
}

func TestShardedScanRejectsCursorsPastTheEnd(t *testing.T) {
	m := &shardedMap[int]{}
	m.set("a", 1)
	keys, examined, next := m.scan(m.scanEnd()+1, 10, nil, nil)
	assert.Empty(t, keys, "a cursor past the end reads as finished, not as an error")
	assert.Zero(t, examined)
	assert.Zero(t, next)
}

func TestShardedDeleteReleasesEmptiedShards(t *testing.T) {
	m := &shardedMap[int]{}
	m.set("only", 1)
	pos, _, _ := m.position("only")
	i := pos / keyPageSlots
	require.NotNil(t, m.pages[i].page)

	assert.True(t, m.del("only"))
	assert.False(t, m.del("only"), "deleting twice reports nothing removed")
	assert.Empty(t, m.pages, "an empty store releases its page directory too")
	assert.Zero(t, m.len())
}

func TestShardedSetReportsOverwrite(t *testing.T) {
	m := &shardedMap[int]{}
	assert.False(t, m.set("k", 1), "the first write is an insert")
	assert.True(t, m.set("k", 2), "the second replaces it")
	assert.Equal(t, 1, m.len(), "an overwrite does not grow the map")
	v, ok := m.get("k")
	assert.True(t, ok)
	assert.Equal(t, 2, v)
}

// Sampling has to find candidates when a handful of keys are spread across all
// the shards, because that is exactly when eviction needs them: a policy with no
// candidates cannot free anything while the budget is already over.
func TestShardedSampleFindsKeysInASparseKeyspace(t *testing.T) {
	for _, keys := range []int{1, 3, 20} {
		m := &shardedMap[int]{}
		for i := 0; i < keys; i++ {
			m.set("k"+strconv.Itoa(i), i)
		}
		for attempt := 0; attempt < 20; attempt++ {
			got := 0
			m.sample(5, func(string, int) { got++ })
			assert.Equal(t, min(5, keys), got,
				"%d keys, attempt %d: sampling must find them", keys, attempt)
		}
	}
}

func TestShardedSampleMovesItsStartingPoint(t *testing.T) {
	m := &shardedMap[int]{}
	for i := 0; i < 500; i++ {
		m.set("key:"+strconv.Itoa(i), i)
	}
	first := map[string]bool{}
	for attempt := 0; attempt < 25; attempt++ {
		taken := 0
		m.sample(1, func(key string, _ int) {
			if taken == 0 {
				first[key] = true
			}
			taken++
		})
	}
	assert.Greater(t, len(first), 1, "consecutive samples must not start in the same place")
}

func TestPagedScanSurvivesGrowthDeletionAndSlotReuse(t *testing.T) {
	m := &shardedMap[int]{}
	for i := 0; i < 300; i++ {
		m.set("stable:"+strconv.Itoa(i), i)
	}
	stable := make(map[string]uint64)
	for _, key := range m.keys() {
		pos, _, _ := m.position(key)
		stable[key] = pos
	}
	cursor := uint64(0)
	seen := make(map[string]int)
	for call := 0; ; call++ {
		keys, examined, next := m.scan(cursor, 7, nil, nil)
		require.LessOrEqual(t, examined, 7)
		for _, key := range keys {
			seen[key]++
		}
		for i := 0; i < 23; i++ {
			key := "new:" + strconv.Itoa(call*23+i)
			m.set(key, i)
		}
		for i := 0; i < 23; i++ {
			m.del("new:" + strconv.Itoa(call*23+i))
		}
		for key, pos := range stable {
			got, _, ok := m.position(key)
			require.True(t, ok)
			require.Equal(t, pos, got)
		}
		require.Less(t, call, 1000)
		if next == 0 {
			break
		}
		cursor = next
	}
	for key := range stable {
		assert.Equal(t, 1, seen[key], key)
	}
}

func TestPagedDeleteReleasesValuesAndReusesSparseSlots(t *testing.T) {
	m := &shardedMap[*int]{}
	for i := 0; i < 3*keyPageSlots; i++ {
		n := i
		m.set(strconv.Itoa(i), &n)
	}
	end := m.end
	for i := keyPageSlots; i < 2*keyPageSlots; i++ {
		m.del(strconv.Itoa(i))
	}
	require.Nil(t, m.pages[1].page)
	for i := 0; i < keyPageSlots; i++ {
		n := i
		m.set("replacement:"+strconv.Itoa(i), &n)
	}
	assert.Equal(t, end, m.end, "vacancies are reused before growing the directory")
	require.NotNil(t, m.pages[1].page)
}

func TestPagedScanHardWorkAndByteTargets(t *testing.T) {
	m := &shardedMap[int]{}
	for i := 0; i < 5000; i++ {
		m.set(strconv.Itoa(i), i)
	}
	_, examined, next := m.scan(0, 1<<20, nil, nil)
	require.LessOrEqual(t, examined, ScanMaxWork)
	require.NotZero(t, next)
	large := &shardedMap[int]{}
	large.set(strings.Repeat("x", ScanByteTarget+1), 1)
	large.set("small", 2)
	keys, examined, next := large.scan(0, 100, nil, nil)
	require.Len(t, keys, 1, "one oversized name is handled alone")
	require.Equal(t, 1, examined)
	require.NotZero(t, next)
	keys, _, next = large.scan(next, 100, nil, nil)
	require.Equal(t, []string{"small"}, keys)
	require.Zero(t, next)
}

func TestPagedHashCollisionsPreserveDistinctKeysAndBoundScan(t *testing.T) {
	m := &shardedMap[int]{hashOverride: func(string) uint64 { return 42 }}
	for i := 0; i < 2000; i++ {
		m.set(strconv.Itoa(i), i)
	}
	for i := 0; i < 2000; i++ {
		got, ok := m.get(strconv.Itoa(i))
		require.True(t, ok)
		require.Equal(t, i, got)
	}
	cursor := uint64(0)
	seen := map[string]bool{}
	for {
		keys, examined, next := m.scan(cursor, 3, nil, nil)
		require.LessOrEqual(t, examined, 3)
		for _, key := range keys {
			seen[key] = true
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	require.Len(t, seen, 2000)
	// Remove an overflow entry and the primary entry, then overwrite another.
	require.True(t, m.del("19"))
	require.True(t, m.del("0"))
	require.True(t, m.set("20", 9000))
	got, ok := m.get("20")
	require.True(t, ok)
	require.Equal(t, 9000, got)
	for _, i := range []int{1, 18, 21, 1999} {
		got, ok = m.get(strconv.Itoa(i))
		require.True(t, ok)
		require.Equal(t, i, got)
	}
}

func TestKeyspaceWalkFreezesLimitsWithoutSnapshottingNames(t *testing.T) {
	ResetKeyspaces()
	defer ResetKeyspaces()
	d := CreateDict()
	RegisterKeyspace(d)
	for i := 0; i < 200; i++ {
		d.Put(strconv.Itoa(i), d.NewObj("v"))
	}
	w := NewKeyspaceWalk()
	require.Len(t, w.ends, 1)
	keys, examined, err := w.Next(5, nil)
	require.NoError(t, err)
	require.LessOrEqual(t, examined, 5)
	for i := 200; i < 2000; i++ {
		d.Put(strconv.Itoa(i), d.NewObj("new"))
	}
	for !w.Done() {
		keys, _, err = w.Next(5, keys)
		require.NoError(t, err)
	}
	require.Len(t, keys, 200, "new high slots are left for dirty reconciliation")
	stale := NewKeyspaceWalk()
	ResetKeyspaces()
	_, _, err = stale.Next(5, nil)
	require.Error(t, err)
}

func FuzzPagedKeyspaceMatchesMap(f *testing.F) {
	f.Add([]byte{0, 0, 0, 1, 0, 2, 2, 0, 3, 1, 1, 5})
	f.Add([]byte{1, 0, 0, 1, 0, 2, 2, 0, 3, 1, 1, 5})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) == 0 {
			return
		}
		if len(data) > 1024 {
			data = data[:1024]
		}
		m := &shardedMap[int]{}
		if data[0]&1 != 0 {
			m.hashOverride = func(string) uint64 { return 7 }
		}
		model := map[string]int{}
		for i := 1; i+2 < len(data); i += 3 {
			key := string([]byte{data[i+1]})
			value := int(data[i+2])
			switch data[i] % 3 {
			case 0:
				_, exists := model[key]
				require.Equal(t, exists, m.set(key, value))
				model[key] = value
			case 1:
				_, exists := model[key]
				require.Equal(t, exists, m.del(key))
				delete(model, key)
			case 2:
				want, exists := model[key]
				got, ok := m.get(key)
				require.Equal(t, exists, ok)
				require.Equal(t, want, got)
			}
			require.Equal(t, len(model), m.len())
		}
		seen := map[string]int{}
		cursor := uint64(0)
		for calls := 0; ; calls++ {
			require.Less(t, calls, 10000)
			keys, examined, next := m.scan(cursor, 3, nil, nil)
			require.LessOrEqual(t, examined, 3)
			for _, key := range keys {
				value, ok := m.get(key)
				require.True(t, ok)
				seen[key] = value
			}
			if next == 0 {
				break
			}
			cursor = next
		}
		require.Equal(t, model, seen)
	})
}
