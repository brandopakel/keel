package data_structure

import (
	"strconv"
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
		require.Less(t, calls, 4*shardCount+16, "the walk must terminate")
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
		require.Less(t, calls, 4*shardCount, "the walk must terminate")
		if next == 0 {
			break
		}
		cursor = next
	}
	assert.Empty(t, kept, "the filter rejected every key")
	assert.Equal(t, 4000, totalExamined, "but every key was still examined")
	assert.Greater(t, calls, 1, "and it took more than one call to do it")
	// One shard of overrun past the budget is the documented bound.
	assert.Less(t, worst, 100+4000/shardCount+64, "no call may examine far past its budget")
}

func TestShardedScanRejectsCursorsPastTheEnd(t *testing.T) {
	m := &shardedMap[int]{}
	m.set("a", 1)
	keys, examined, next := m.scan(shardCount, 10, nil, nil)
	assert.Empty(t, keys, "a cursor past the end reads as finished, not as an error")
	assert.Zero(t, examined)
	assert.Zero(t, next)
}

func TestShardedDeleteReleasesEmptiedShards(t *testing.T) {
	m := &shardedMap[int]{}
	m.set("only", 1)
	i := shardOf("only")
	require.NotNil(t, m.shards[i])

	assert.True(t, m.del("only"))
	assert.False(t, m.del("only"), "deleting twice reports nothing removed")
	assert.Nil(t, m.shards[i], "an emptied shard gives its map back")
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

// Sampling has to find candidates when a handful of keys are spread across a
// thousand shards, because that is exactly when eviction needs them: a policy
// with no candidates cannot free anything while the budget is already over.
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

func TestShardOfIsStableForAKey(t *testing.T) {
	// Membership has to hold still for as long as a cursor is live, which is
	// the whole basis for treating a shard index as a resumable position.
	for i := 0; i < 200; i++ {
		key := "key:" + strconv.Itoa(i)
		want := shardOf(key)
		for repeat := 0; repeat < 5; repeat++ {
			assert.Equal(t, want, shardOf(key), "%s must not move between calls", key)
		}
		assert.Less(t, want, shardCount)
		assert.GreaterOrEqual(t, want, 0)
	}
}

func TestShardOfSpreadsKeysAcrossShards(t *testing.T) {
	used := map[int]int{}
	for i := 0; i < 10000; i++ {
		used[shardOf("key:"+strconv.Itoa(i))]++
	}
	// A hash that piled keys into a few shards would make the cursor useless,
	// because a shard is taken whole and so bounds the work of one call.
	assert.Greater(t, len(used), shardCount/2, "keys must reach most shards")
	worst := 0
	for _, n := range used {
		if n > worst {
			worst = n
		}
	}
	assert.Less(t, worst, 60, "no shard may take a large share of 10,000 keys")
}
