package data_structure

import (
	"runtime"
	"strconv"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"
)

func TestSetChurnReleasesSparseMembershipMap(t *testing.T) {
	s := NewSet()
	for i := 0; i < 100000; i++ {
		s.Add(strconv.Itoa(i))
	}
	for i := 1000; i < 100000; i++ {
		s.Remove(strconv.Itoa(i))
	}
	before := heapBytes()
	if compact, ok := any(s).(interface{ CompactIndex(int) int }); ok {
		for i := 0; i < 100; i++ {
			require.LessOrEqual(t, compact.CompactIndex(37), 37)
		}
	}
	after := heapBytes()
	t.Logf("retained=%d after=%d recovered=%d", before, after, int64(before)-int64(after))
	require.Greater(t, int64(before)-int64(after), int64(1<<20))
	for i := 0; i < 1000; i++ {
		require.True(t, s.Contains(strconv.Itoa(i)))
	}
	runtime.KeepAlive(s)
	t.Logf("set struct bytes=%d; old 40 and new 48 both fit the 48-byte Go allocation class", unsafe.Sizeof(Set{}))
}

func sparseSet(t *testing.T) *Set {
	t.Helper()
	s := NewSet()
	for i := 0; i < 8192; i++ {
		s.Add(strconv.Itoa(i))
	}
	for i := 2048; i < 8192; i++ {
		s.Remove(strconv.Itoa(i))
	}
	require.NotNil(t, s.compaction)
	return s
}

func requireSetIndex(t *testing.T, s *Set) {
	t.Helper()
	require.Equal(t, len(s.order), len(s.index))
	for index, member := range s.order {
		position, ok := s.index[member]
		require.True(t, ok)
		require.Equal(t, index, position)
	}
}

func TestSetCompactionMirrorsMovesShufflesAndInsertions(t *testing.T) {
	s := sparseSet(t)
	require.Equal(t, 37, s.CompactIndex(37))
	expected := make(map[string]bool)
	for _, member := range s.order {
		expected[member] = true
	}
	for turn := 0; s.compaction != nil && turn < 1000; turn++ {
		removed := strconv.Itoa(turn)
		s.Remove(removed)
		delete(expected, removed)
		added := "new:" + strconv.Itoa(turn)
		s.Add(added)
		expected[added] = true
		s.ShufflePrefix(1000)
		require.LessOrEqual(t, s.CompactIndex(13), 13)
		requireSetIndex(t, s)
	}
	require.Nil(t, s.compaction)
	require.Equal(t, len(expected), s.Len())
	for member := range expected {
		require.True(t, s.Contains(member))
	}
}

func TestSetCompactionCancelsOnRegrowth(t *testing.T) {
	s := sparseSet(t)
	require.Equal(t, 1, s.CompactIndex(1))
	for i := 0; i < 8192; i++ {
		s.Add("new:" + strconv.Itoa(i))
	}
	require.Nil(t, s.compaction)
	requireSetIndex(t, s)
}

func TestSetCompactionRestartsAfterFurtherLargeShrink(t *testing.T) {
	s := sparseSet(t)
	require.Equal(t, 1024, s.CompactIndex(1024))
	previous := s.compaction
	for i := 10; i < 2048; i++ {
		s.Remove(strconv.Itoa(i))
	}
	require.NotSame(t, previous, s.compaction, "do not retain a mostly empty shadow map after another large shrink")
	for i := 0; s.compaction != nil && i < 10; i++ {
		require.LessOrEqual(t, s.CompactIndex(3), 3)
	}
	require.Nil(t, s.compaction)
	requireSetIndex(t, s)
	require.Equal(t, 10, s.Len())
}

func TestSetCompactionUsesSharedKeyAndMemberBudget(t *testing.T) {
	store := NewKeyed[*Set]("sets")
	sets := []*Set{sparseSet(t), sparseSet(t), sparseSet(t)}
	for i, set := range sets {
		store.Put(strconv.Itoa(i), set)
	}
	for i := 0; i < 1000; i++ {
		require.LessOrEqual(t, store.CompactValues(37), 37)
	}
	for _, set := range sets {
		require.Nil(t, set.compaction)
		requireSetIndex(t, set)
	}
}
