package core

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/brandopakel/keel/internal/config"
	"github.com/brandopakel/keel/internal/data_structure"
)

// withBudget puts every keyspace under a byte budget for one test.
func withBudget(t *testing.T, bytes uint64, policy int) {
	t.Helper()
	mm, ks, es := config.MaxMemory, config.KeyNumberLimit, config.EvictStrategy
	t.Cleanup(func() {
		config.MaxMemory, config.KeyNumberLimit, config.EvictStrategy = mm, ks, es
		ResetStores()
	})
	config.MaxMemory, config.KeyNumberLimit, config.EvictStrategy = bytes, 100000000, policy
	ResetStores()
}

// TestEveryKeyspaceIsAccounted is the point of the change. Before it, only the
// string dictionary was measured, so a keyspace of HyperLogLogs - 12KB each,
// whatever their cardinality - could run past -maxmemory without anything
// noticing, and eviction had nothing to evict because it only looked at strings.
func TestEveryKeyspaceIsAccounted(t *testing.T) {
	cases := []struct {
		name string
		fill func(i int)
	}{
		{"string", func(i int) { cmdSET([]string{"s" + strconv.Itoa(i), strings.Repeat("v", 500)}) }},
		{"set", func(i int) { cmdSADD([]string{"set" + strconv.Itoa(i), "a", "b", "c", "d", "e"}) }},
		{"sorted set", func(i int) { cmdZADD([]string{"z" + strconv.Itoa(i), "1", "a", "2", "b"}) }},
		{"hyperloglog", func(i int) { cmdPFADD([]string{"h" + strconv.Itoa(i), "x"}) }},
		{"cuckoo filter", func(i int) { cmdCFADD([]string{"c" + strconv.Itoa(i), "x"}) }},
		{"count-min sketch", func(i int) { cmdCMSINITBYDIM([]string{"m" + strconv.Itoa(i), "200", "5"}) }},
		{"bloom filter", func(i int) { cmdBFMADD([]string{"b" + strconv.Itoa(i), "x"}) }},
	}

	const budget = 1 << 20 // 1 MB
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withBudget(t, budget, config.LRU)
			// Enough of every type to exceed the budget: the smallest of
			// them is a few hundred bytes, so 2000 keys would simply fit and
			// eviction would never run.
			for i := 0; i < 6000; i++ {
				c.fill(i)
			}
			used := data_structure.TotalMemUsed()
			assert.LessOrEqual(t, used, uint64(budget)*11/10,
				"%s keyspace ran to %d bytes against a %d budget", c.name, used, budget)
			assert.Greater(t, data_structure.Evicted(), uint64(0),
				"%s: eviction should have run", c.name)
			t.Logf("%-18s held %d keys in %d bytes", c.name, data_structure.TotalKeys(), used)
		})
	}
}

// TestBudgetIsSharedAcrossKeyspaces checks that the bound is one budget rather
// than one per store: filling several types together must still total under it.
func TestBudgetIsSharedAcrossKeyspaces(t *testing.T) {
	withBudget(t, 1<<20, config.LRU)
	for i := 0; i < 1000; i++ {
		n := strconv.Itoa(i)
		cmdSET([]string{"s" + n, strings.Repeat("v", 500)})
		cmdSADD([]string{"set" + n, "a", "b", "c"})
		cmdPFADD([]string{"h" + n, "x"})
		cmdZADD([]string{"z" + n, "1", "a"})
	}
	used := data_structure.TotalMemUsed()
	assert.LessOrEqual(t, used, uint64(1<<20)*11/10,
		"the budget must span every keyspace, not apply to each separately")
	t.Logf("mixed keyspaces: %d keys in %d bytes", data_structure.TotalKeys(), used)
}

// TestEvictionCrossesKeyspaces checks that pressure in one keyspace can free a
// key from another. Without it, a store full of sketches would be untouchable
// and the budget could only be met by destroying the strings.
func TestEvictionCrossesKeyspaces(t *testing.T) {
	withBudget(t, 512<<10, config.LRU)

	// Fill with sketches only, so everything the budget holds is one type.
	for i := 0; i < 200; i++ {
		cmdPFADD([]string{"h" + strconv.Itoa(i), "x"})
	}
	hllKeys := hllStore.Len()
	assert.Greater(t, hllKeys, 0)

	// Now write strings. The budget is already full of sketches, so room can
	// only come from evicting them.
	for i := 0; i < 500; i++ {
		cmdSET([]string{"s" + strconv.Itoa(i), strings.Repeat("v", 200)})
	}
	assert.Less(t, hllStore.Len(), hllKeys,
		"pressure from the string keyspace must be able to evict sketches")
	assert.LessOrEqual(t, data_structure.TotalMemUsed(), uint64(512<<10)*11/10)
}

// TestSetGrowthIsRemeasured covers the Resize hook. A set's size changes as
// members are added, without going through Put, so the keyspace has to be told
// - and if it is not, the budget quietly believes an old, smaller figure.
func TestSetGrowthIsRemeasured(t *testing.T) {
	withBudget(t, 0, config.LRU) // unbounded, so nothing is evicted mid-test

	cmdSADD([]string{"s", "a"})
	small := setStore.MemUsed()

	for i := 0; i < 5000; i++ {
		cmdSADD([]string{"s", "member:" + strconv.Itoa(i)})
	}
	grown := setStore.MemUsed()

	assert.Greater(t, grown, small+5000*20,
		"adding 5000 members must be reflected in the keyspace's accounting")

	cmdSREM([]string{"s", "member:1", "member:2", "member:3"})
	assert.Less(t, setStore.MemUsed(), grown, "removing members must shrink it again")
}

// TestMemoryUsageFindsKeysInEveryKeyspace covers the reporting side. MEMORY
// USAGE looked only in the string dictionary at first, so it reported nil for
// every set, sketch and filter - which is exactly the blind spot this change is
// about, reproduced in the tool meant to reveal it.
func TestMemoryUsageFindsKeysInEveryKeyspace(t *testing.T) {
	withBudget(t, 0, config.LRU)

	cmdSET([]string{"str", strings.Repeat("v", 100)})
	cmdSADD([]string{"set", "a", "b", "c"})
	cmdZADD([]string{"zset", "1", "a"})
	cmdPFADD([]string{"hll", "x"})
	cmdCFADD([]string{"cf", "x"})
	cmdCMSINITBYDIM([]string{"cms", "200", "5"})
	cmdBFMADD([]string{"bf", "x"})

	for _, key := range []string{"str", "set", "zset", "hll", "cf", "cms", "bf"} {
		res, err := Decode(cmdMEMORY([]string{"USAGE", key}))
		assert.Nil(t, err)
		n, ok := res.(int64)
		assert.True(t, ok, "MEMORY USAGE %s should report a size, got %v", key, res)
		assert.Greater(t, n, int64(0), "MEMORY USAGE %s reported %d", key, n)
	}

	// A dense HyperLogLog is 12KB whatever it holds, and must be reported as
	// such rather than as the one item added to it.
	res, _ := Decode(cmdMEMORY([]string{"USAGE", "hll"}))
	assert.Greater(t, res.(int64), int64(12000))
}

// TestBloomFilterSizeIsTheBitArray guards a bug that was in the tree before
// this change: BF.INFO reported reflect.TypeOf(*sb).Size(), the struct header,
// so a filter holding a hundred kilobytes of bits reported about forty bytes.
// A memory budget built on that figure would have been meaningless.
func TestBloomFilterSizeIsTheBitArray(t *testing.T) {
	withBudget(t, 0, config.LRU)

	cmdBFRESERVE([]string{"bf", "0.01", "100000"})
	cmdBFMADD([]string{"bf", "x"})

	res, _ := Decode(cmdMEMORY([]string{"USAGE", "bf"}))
	assert.Greater(t, res.(int64), int64(100000),
		"a filter sized for 100,000 items at 1%% error holds ~120KB of bits")
}
