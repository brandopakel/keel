package core

import (
	"strconv"
	"testing"

	"github.com/brandopakel/keel/internal/config"
	"github.com/brandopakel/keel/internal/data_structure"
)

func TestHLLGrowthIsChargedAndEnforcesBudget(t *testing.T) {
	withBudget(t, 4096, config.LRU)
	run(t, "PFADD", "growing", "first")
	initial := data_structure.TotalMemUsed()
	args := []string{"growing"}
	for i := 0; i < 100; i++ {
		args = append(args, strconv.Itoa(i))
	}
	run(t, "PFADD", args...)
	if data_structure.TotalMemUsed() <= initial {
		t.Fatal("sparse growth was not charged")
	}
	args = []string{"growing"}
	for i := 100; i < 1000; i++ {
		args = append(args, strconv.Itoa(i))
	}
	run(t, "PFADD", args...)
	if data_structure.TotalMemUsed() > 4096 || run(t, "EXISTS", "growing") != int64(0) {
		t.Fatal("promotion to a dense sketch bypassed the keyspace budget")
	}
}
