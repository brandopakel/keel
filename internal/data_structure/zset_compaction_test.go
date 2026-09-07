package data_structure

import (
	"math"
	"math/rand"
	"runtime"
	"strconv"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"
)

func sparseZSet(t *testing.T) *ZSet {
	t.Helper()
	z := CreateZSet()
	for i := 0; i < 8192; i++ {
		z.Add(float64(i), strconv.Itoa(i), 0)
	}
	for i := 2048; i < 8192; i++ {
		require.True(t, z.Remove(strconv.Itoa(i)))
	}
	require.NotNil(t, z.indexState)
	require.NotNil(t, z.indexState.next)
	return z
}

func requireZSetIndex(t *testing.T, z *ZSet, expected map[string]float64) {
	t.Helper()
	require.Equal(t, expected, z.dict)
	rank := 0
	for node := z.sl.first(); node != nil; node = node.next() {
		score, ok := expected[node.ele]
		require.True(t, ok)
		require.Equal(t, score, node.score)
		got, ok := z.Rank(node.ele, false)
		require.True(t, ok)
		require.Equal(t, int64(rank), got)
		rank++
	}
	require.Equal(t, len(expected), rank)
}

func TestZSetCompactionReleasesSparseScoreMap(t *testing.T) {
	z := CreateZSet()
	for i := 0; i < 100000; i++ {
		z.Add(float64(i), strconv.Itoa(i), 0)
	}
	for i := 1000; i < 100000; i++ {
		z.Remove(strconv.Itoa(i))
	}
	require.Equal(t, 1000, z.Len())
	require.NotNil(t, z.indexState.next)
	before := heapBytes()
	work := 0
	for i := 0; z.indexState != nil && z.indexState.next != nil && i < 100; i++ {
		used := z.CompactIndex(37)
		require.LessOrEqual(t, used, 37)
		work += used
	}
	require.Nil(t, z.indexState, "small compacted collection releases tracking state")
	require.Equal(t, 1000, work)
	after := heapBytes()
	require.Greater(t, int64(before)-int64(after), int64(1<<20))
	for i := 0; i < 1000; i++ {
		score, ok := z.Score(strconv.Itoa(i))
		require.True(t, ok)
		require.Equal(t, float64(i), score)
	}
	t.Logf("recovered=%d bytes; zset struct=%d bytes (baseline 24)", int64(before)-int64(after), unsafe.Sizeof(ZSet{}))
	runtime.KeepAlive(z)
}

func TestZSetCompactionMirrorsCursorDeletionRescoresAndInsertions(t *testing.T) {
	z := sparseZSet(t)
	expected := make(map[string]float64, z.Len())
	for key, score := range z.dict {
		expected[key] = score
	}
	require.Equal(t, 37, z.CompactIndex(37))
	removed := z.indexState.cursor.ele
	require.True(t, z.Remove(removed))
	delete(expected, removed)
	rescored := z.indexState.cursor.ele
	z.Add(math.Inf(1), rescored, 0)
	expected[rescored] = math.Inf(1)
	z.Add(math.Inf(-1), removed, 0)
	expected[removed] = math.Inf(-1)
	rng := rand.New(rand.NewSource(73))
	for turn := 0; z.indexState.next != nil && turn < 1000; turn++ {
		key := strconv.Itoa(rng.Intn(2500))
		if turn%3 == 0 {
			z.Remove(key)
			delete(expected, key)
		} else {
			score := float64(rng.Intn(50) - 25)
			z.Add(score, key, 0)
			expected[key] = score
		}
		require.LessOrEqual(t, z.CompactIndex(13), 13)
		requireZSetIndex(t, z, expected)
	}
	require.Nil(t, z.indexState.next)
	require.Equal(t, z.Len(), z.indexState.peak, "walk must publish, not abandon")
	requireZSetIndex(t, z, expected)
}

func TestZSetCompactionRestartsOnFurtherShrinkAndDropsEmptyMap(t *testing.T) {
	z := sparseZSet(t)
	require.Equal(t, ScanMaxWork, z.CompactIndex(1<<20))
	for i := 10; i < 2048; i++ {
		z.Remove(strconv.Itoa(i))
	}
	require.Equal(t, 10, z.Len())
	work := 0
	for z.indexState != nil && work < 20 {
		work += z.CompactIndex(3)
	}
	require.Equal(t, 10, work)
	require.Nil(t, z.indexState)

	z = sparseZSet(t)
	for i := 0; i < 2048; i++ {
		z.Remove(strconv.Itoa(i))
	}
	require.Nil(t, z.indexState)
	require.Zero(t, z.Len())
	require.Equal(t, ZAddAdded, z.Add(1, "new", 0))
	requireZSetIndex(t, z, map[string]float64{"new": 1})
}

func TestZSetCompactionCancelsRegrowthAndBoundsChurningWalk(t *testing.T) {
	t.Run("regrowth", func(t *testing.T) {
		z := sparseZSet(t)
		z.CompactIndex(1)
		for i := 8192; i < 13000; i++ {
			z.Add(float64(i), strconv.Itoa(i), 0)
		}
		require.Nil(t, z.indexState.next)
		require.Nil(t, z.indexState.cursor)
		require.Equal(t, 6856, z.Len())
	})
	t.Run("bounded visits", func(t *testing.T) {
		z := sparseZSet(t)
		visited := 0
		for i := 0; z.indexState.next != nil && i < 5000; i++ {
			// Keep population constant while extending the tail ahead of the
			// cursor. Every inserted value is mirrored, but the walk must stop.
			first := z.sl.first().ele
			z.Remove(first)
			z.Add(float64(8192+i), "new:"+strconv.Itoa(i), 0)
			visited += z.CompactIndex(1)
		}
		require.Equal(t, 4096, visited)
		require.Nil(t, z.indexState.next)
		require.Nil(t, z.indexState.cursor)
		require.Equal(t, 8192, z.indexState.peak, "incomplete shadow must not be published")
		require.Equal(t, 2048, z.Len())
		for i := 2048; i < 4096; i++ {
			score, ok := z.Score("new:" + strconv.Itoa(i))
			require.True(t, ok)
			require.Equal(t, float64(8192+i), score)
		}
	})
}

func TestZSetCompactionKeepsSmallCollectionsUntracked(t *testing.T) {
	z := CreateZSet()
	for i := 0; i < 1000; i++ {
		z.Add(float64(i), strconv.Itoa(i), 0)
	}
	require.Nil(t, z.indexState)
	require.Zero(t, z.CompactIndex(1000))
	require.Zero(t, z.CompactIndex(-1))
	z = sparseZSet(t)
	require.Zero(t, z.CompactIndex(0))
	require.Equal(t, 2048, z.Len())
}
