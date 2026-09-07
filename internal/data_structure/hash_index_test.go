package data_structure

import (
	"math/bits"
	"math/rand"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func requireBoundedHash(t *testing.T, h *Hash, expected map[string]string) {
	t.Helper()
	require.Equal(t, len(expected), h.Len())
	seen := make(map[string]string, len(expected))
	h.Visit(func(key, value string) bool {
		_, duplicate := seen[key]
		require.False(t, duplicate)
		seen[key] = value
		return true
	})
	require.Equal(t, expected, seen)
	for field, value := range expected {
		got, ok := h.Get(field)
		require.True(t, ok)
		require.Equal(t, value, got)
	}
	seen = make(map[string]string, len(expected))
	for cursor := h.Cursor(); ; cursor.Advance() {
		field, value, ok := cursor.Entry()
		if !ok {
			break
		}
		_, duplicate := seen[field]
		require.False(t, duplicate)
		seen[field] = value
	}
	require.Equal(t, expected, seen)
	if h.large == nil {
		require.LessOrEqual(t, len(h.fields), hashLeafFields)
		return
	}
	var check func(*hashNode, uint64)
	check = func(node *hashNode, used uint64) {
		require.NotNil(t, node)
		if node.fields == nil {
			require.Equal(t, 1, bits.OnesCount64(node.bit))
			require.Zero(t, used&node.bit, "a routing bit cannot repeat along a path")
			check(node.left, used|node.bit)
			check(node.right, used|node.bit)
			return
		}
		for node != nil {
			require.NotEmpty(t, node.fields)
			require.LessOrEqual(t, len(node.fields), hashLeafFields)
			require.LessOrEqual(t, node.peak, hashLeafFields)
			require.GreaterOrEqual(t, node.peak, len(node.fields))
			node = node.next
		}
	}
	check(h.large.root, 0)
}

func TestHashLeavesSplitShrinkAndReturnToSmallStorage(t *testing.T) {
	h := NewHash()
	expected := map[string]string{}
	for cycle := 0; cycle < 3; cycle++ {
		for i := 0; i < 20000; i++ {
			key, value := strconv.Itoa(i), "value:"+strconv.Itoa(cycle)
			h.Set(key, value)
			expected[key] = value
		}
		require.NotNil(t, h.large)
		requireBoundedHash(t, h, expected)
		for i := 1; i < 20000; i++ {
			key := strconv.Itoa(i)
			require.Equal(t, 1, h.Del(key, key), "repeated removal must count once")
			delete(expected, key)
		}
		require.Nil(t, h.large)
		requireBoundedHash(t, h, expected)
	}
	require.Equal(t, 1, h.Del("0"))
	requireBoundedHash(t, h, map[string]string{})
	h.Set("reused", "ok")
	requireBoundedHash(t, h, map[string]string{"reused": "ok"})
}

func TestHashLeavesRandomMutationsAndBinaryFields(t *testing.T) {
	h := NewHash()
	expected := map[string]string{}
	rng := rand.New(rand.NewSource(923))
	for i := 0; i < 30000; i++ {
		field := strings.Repeat("shared:", 30) + "\x00" + strconv.Itoa(rng.Intn(5000))
		if i%4 == 0 {
			_, exists := expected[field]
			removed := h.Del(field)
			if exists {
				require.Equal(t, 1, removed)
			} else {
				require.Zero(t, removed)
			}
			delete(expected, field)
		} else {
			value := "\x00value:" + strconv.Itoa(i)
			_, exists := expected[field]
			require.Equal(t, !exists, h.Set(field, value))
			expected[field] = value
		}
		if i%997 == 0 {
			requireBoundedHash(t, h, expected)
		}
	}
	requireBoundedHash(t, h, expected)
	visited := 0
	h.Visit(func(_, _ string) bool { visited++; return false })
	require.Equal(t, 1, visited)
}

func TestHashIndexExactCollisionsStayInBoundedLeaves(t *testing.T) {
	h := &hashIndex{}
	zeroHash := func(string) uint64 { return 0 }
	for i := 0; i < 1000; i++ {
		h.setByHash(strconv.Itoa(i), "value", 0, zeroHash)
	}
	require.Equal(t, 1000, h.count)
	for node := h.root; node != nil; node = node.next {
		require.LessOrEqual(t, len(node.fields), hashLeafFields)
	}
	// Route a different hash beside the collision chain without redistributing
	// all colliding fields, then remove and update within that retained chain.
	h.setByHash("different", "branch", 1, zeroHash)
	h.setByHash("193", "updated", 0, zeroHash)
	for i := 0; i < 999; i++ {
		h.delByHash(strconv.Itoa(i), 0)
	}
	require.Equal(t, 2, h.count)
	value, ok := h.getByHash("999", 0)
	require.True(t, ok)
	require.Equal(t, "value", value)
	value, ok = h.getByHash("different", 1)
	require.True(t, ok)
	require.Equal(t, "branch", value)
	h.delByHash("999", 0)
	h.delByHash("different", 1)
	require.Nil(t, h.root)
	require.Zero(t, h.count)
}
