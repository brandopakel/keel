package core

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"memkv/internal/config"
	"memkv/internal/constant"
	"memkv/internal/data_structure"
)

func resetDictStore() {
	dictStore = data_structure.CreateDict()
}

func TestCmdMemoryUsage(t *testing.T) {
	resetDictStore()
	cmdSET([]string{"small", "v"})
	cmdSET([]string{"large", strings.Repeat("v", 5000)})

	small, _ := Decode(cmdMEMORY([]string{"USAGE", "small"}))
	large, _ := Decode(cmdMEMORY([]string{"USAGE", "large"}))

	assert.Greater(t, small.(int64), int64(0))
	assert.Greater(t, large.(int64), small.(int64)+4000,
		"a 5000-byte value must be accounted far above a 1-byte one")
}

func TestCmdMemoryUsageOnMissingKey(t *testing.T) {
	resetDictStore()
	// Asserted on the wire bytes rather than the decoded value: DecodeOne maps
	// the RESP null bulk string to an empty string, so a decoded comparison
	// could not tell nil from a zero-length reply.
	assert.Equal(t, constant.RespNil, cmdMEMORY([]string{"USAGE", "nosuchkey"}),
		"a missing key reports nil, not zero")
}

func TestCmdMemoryRejectsUnknownSubcommand(t *testing.T) {
	resetDictStore()
	res, _ := Decode(cmdMEMORY([]string{"DOCTOR"}))
	assert.Contains(t, res, "unknown MEMORY subcommand")

	res, _ = Decode(cmdMEMORY([]string{}))
	assert.Contains(t, res, "wrong number of arguments")

	res, _ = Decode(cmdMEMORY([]string{"USAGE"}))
	assert.Contains(t, res, "wrong number of arguments")
}

func TestCmdInfoReportsMemoryAndKeyspace(t *testing.T) {
	resetDictStore()
	old := config.MaxMemory
	config.MaxMemory = 1 << 20
	t.Cleanup(func() { config.MaxMemory = old })

	cmdSET([]string{"a", "1"})
	cmdSET([]string{"b", "2"})

	res, err := Decode(cmdINFO([]string{}))
	assert.Nil(t, err)
	out := res.(string)

	assert.Contains(t, out, "# Memory")
	assert.Contains(t, out, "maxmemory:1048576")
	assert.Contains(t, out, "db0:keys=2")
	assert.Contains(t, out, "evicted_keys:")
	assert.Contains(t, out, "used_memory:")
}

func TestCmdInfoSectionFiltering(t *testing.T) {
	resetDictStore()
	res, _ := Decode(cmdINFO([]string{"memory"}))
	out := res.(string)
	assert.Contains(t, out, "# Memory")
	assert.NotContains(t, out, "# Keyspace", "a section filter must exclude the others")

	res, _ = Decode(cmdINFO([]string{"keyspace"}))
	out = res.(string)
	assert.Contains(t, out, "# Keyspace")
	assert.NotContains(t, out, "# Memory")
}

func TestCmdInfoReportsTheActivePolicy(t *testing.T) {
	resetDictStore()
	old := config.EvictStrategy
	t.Cleanup(func() { config.EvictStrategy = old })

	for strategy, want := range map[int]string{
		config.LRU:        "allkeys-lru",
		config.LFU:        "allkeys-lfu",
		config.EvictFirst: "allkeys-random",
	} {
		config.EvictStrategy = strategy
		res, _ := Decode(cmdINFO([]string{"memory"}))
		assert.Contains(t, res.(string), "maxmemory_policy:"+want)
	}
}

func TestHumanBytes(t *testing.T) {
	for _, c := range []struct {
		in   uint64
		want string
	}{
		{0, "0B"},
		{512, "512B"},
		{1024, "1.00K"},
		{1536, "1.50K"},
		{1 << 20, "1.00M"},
		{4 << 20, "4.00M"},
		{1 << 30, "1.00G"},
	} {
		assert.Equal(t, c.want, humanBytes(c.in), "humanBytes(%d)", c.in)
	}
}

func TestCmdDbsize(t *testing.T) {
	resetDictStore()
	res, _ := Decode(cmdDBSIZE([]string{}))
	assert.EqualValues(t, 0, res)

	cmdSET([]string{"a", "1"})
	cmdSET([]string{"b", "2"})
	res, _ = Decode(cmdDBSIZE([]string{}))
	assert.EqualValues(t, 2, res)

	res, _ = Decode(cmdDBSIZE([]string{"extra"}))
	assert.Contains(t, res, "wrong number of arguments")
}
