package data_structure

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLRMCountsModificationsNotReads: under an LRM policy a read leaves a
// key's score where it was, and a store, a resize or a note of a change in
// place moves it past every score before; under LRU a read moves it too, and
// a note of a change in place adds nothing to what the read did; under LFU a
// note changes nothing.
func TestLRMCountsModificationsNotReads(t *testing.T) {
	t.Parallel()
	score := func(ks Keyspace, key string) uint64 {
		s, ok := ks.ScoreOf(key)
		require.True(t, ok, key)
		return s
	}
	for _, policy := range []EvictionPolicy{EvictLRM, EvictVolatileLRM} {
		space := NewSpace(Limits{Eviction: policy})
		dict := CreateDict(space)
		sets := NewKeyed[*Set](space, "set")
		space.RegisterKeyspace(dict)
		space.RegisterKeyspace(sets)
		dict.Put("s", dict.NewObj("v"))
		sets.Put("set", NewSet())

		held, heldSet := score(dict, "s"), score(sets, "set")
		dict.Get("s")
		sets.Get("set")
		assert.Equal(t, held, score(dict, "s"), "a read is not a modification")
		assert.Equal(t, heldSet, score(sets, "set"))

		dict.UpdateValue("s", "w")
		assert.Greater(t, score(dict, "s"), heldSet)
		held = score(dict, "s")
		set, _ := sets.Get("set")
		set.Add("m")
		sets.Resize("set")
		assert.Greater(t, score(sets, "set"), held)
		held = score(sets, "set")
		dict.Modified("s")
		assert.Greater(t, score(dict, "s"), held)
		held = score(dict, "s")
		sets.Modified("set")
		assert.Greater(t, score(sets, "set"), held)
	}

	space := NewSpace(Limits{Eviction: EvictLRU})
	dict := CreateDict(space)
	space.RegisterKeyspace(dict)
	dict.Put("a", dict.NewObj("v"))
	dict.Put("b", dict.NewObj("v"))
	dict.Get("a")
	assert.Greater(t, score(dict, "a"), score(dict, "b"), "under LRU a read is a use")
	held := score(dict, "b")
	dict.Modified("b")
	assert.Equal(t, held, score(dict, "b"), "the command's own lookup counted it already")

	space = NewSpace(Limits{Eviction: EvictLFU})
	dict = CreateDict(space)
	space.RegisterKeyspace(dict)
	dict.Put("a", dict.NewObj("v"))
	held = score(dict, "a")
	dict.Modified("a")
	assert.Equal(t, held, score(dict, "a"), "a change in place is not another use under LFU")
}

// TestLRMStampsModificationsUnderNoTouch: under CLIENT NO-TOUCH an LRM space
// still stamps a write, as Redis's keyModified stamps a key whatever the
// client's mode, and a read still leaves the stamp alone; an LRU space's
// write records nothing under no-touch, as before.
func TestLRMStampsModificationsUnderNoTouch(t *testing.T) {
	t.Parallel()
	score := func(ks Keyspace, key string) uint64 {
		s, ok := ks.ScoreOf(key)
		require.True(t, ok, key)
		return s
	}
	space := NewSpace(Limits{Eviction: EvictLRM})
	dict := CreateDict(space)
	space.RegisterKeyspace(dict)
	dict.Put("a", dict.NewObj("v"))
	held := score(dict, "a")
	space.SetNoTouch(true)
	dict.Get("a")
	assert.Equal(t, held, score(dict, "a"), "a read, no-touch or not")
	dict.Put("a", dict.NewObj("w"))
	assert.Greater(t, score(dict, "a"), held, "a write under no-touch")
	space.SetNoTouch(false)

	space = NewSpace(Limits{Eviction: EvictLRU})
	dict = CreateDict(space)
	space.RegisterKeyspace(dict)
	dict.Put("a", dict.NewObj("v"))
	held = score(dict, "a")
	space.SetNoTouch(true)
	obj := dict.Get("a")
	dict.Put("a", obj)
	assert.Equal(t, held, score(dict, "a"), "LRU records nothing under no-touch")
}
