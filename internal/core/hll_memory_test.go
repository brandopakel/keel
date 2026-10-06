package core

import (
	"strconv"
	"testing"
)

func TestHLLGrowthIsChargedAndEnforcesBudget(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	withBudget(t, e, 4096, EvictLRU)
	runOn(t, e, "PFADD", "growing", "first")
	initial := e.space.TotalMemUsed()
	args := []string{"growing"}
	for i := 0; i < 100; i++ {
		args = append(args, strconv.Itoa(i))
	}
	runOn(t, e, "PFADD", args...)
	if e.space.TotalMemUsed() <= initial {
		t.Fatal("sparse growth was not charged")
	}
	args = []string{"growing"}
	for i := 100; i < 1000; i++ {
		args = append(args, strconv.Itoa(i))
	}
	runOn(t, e, "PFADD", args...)
	if e.space.TotalMemUsed() > 4096 || runOn(t, e, "EXISTS", "growing") != int64(0) {
		t.Fatal("promotion to a dense sketch bypassed the keyspace budget")
	}
}
