package core

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/brandopakel/keel/internal/constant"
)

func TestCmdMemoryUsage(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	e.cmdSET([]string{"small", "v"})
	e.cmdSET([]string{"large", strings.Repeat("v", 5000)})

	small, _ := Decode(e.cmdMEMORY([]string{"USAGE", "small"}))
	large, _ := Decode(e.cmdMEMORY([]string{"USAGE", "large"}))

	assert.Greater(t, small.(int64), int64(0))
	assert.Greater(t, large.(int64), small.(int64)+4000,
		"a 5000-byte value must be accounted far above a 1-byte one")
}

func TestCmdMemoryUsageOnMissingKey(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	// Asserted on the wire bytes rather than the decoded value: DecodeOne maps
	// the RESP null bulk string to an empty string, so a decoded comparison
	// could not tell nil from a zero-length reply.
	assert.Equal(t, constant.RespNil, e.cmdMEMORY([]string{"USAGE", "nosuchkey"}),
		"a missing key reports nil, not zero")
}

func TestCmdMemoryRejectsUnknownSubcommand(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	res, _ := Decode(e.cmdMEMORY([]string{"DOCTOR"}))
	assert.Equal(t, "ERR unknown subcommand 'DOCTOR'. Try MEMORY HELP.", res)

	res, _ = Decode(e.cmdMEMORY([]string{}))
	assert.Contains(t, res, "wrong number of arguments")

	res, _ = Decode(e.cmdMEMORY([]string{"USAGE"}))
	assert.Contains(t, res, "wrong number of arguments")
}

func TestCmdInfoReportsMemoryAndKeyspace(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{MaxMemory: 1 << 20})

	e.cmdSET([]string{"a", "1"})
	e.cmdSET([]string{"b", "2"})

	res, err := Decode(e.cmdINFO([]string{}))
	assert.Nil(t, err)
	out := res.(string)

	assert.Contains(t, out, "# Memory")
	assert.Contains(t, out, "maxmemory:1048576")
	assert.Contains(t, out, "db0:keys=2")
	assert.Contains(t, out, "evicted_keys:")
	assert.Contains(t, out, "used_memory:")
}

func TestCmdInfoSectionFiltering(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	res, _ := Decode(e.cmdINFO([]string{"memory"}))
	out := res.(string)
	assert.Contains(t, out, "# Memory")
	assert.NotContains(t, out, "# Keyspace", "a section filter must exclude the others")

	res, _ = Decode(e.cmdINFO([]string{"keyspace"}))
	out = res.(string)
	assert.Contains(t, out, "# Keyspace")
	assert.NotContains(t, out, "# Memory")
}

func TestCmdInfoReportsTheActivePolicy(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	for policy, want := range map[EvictionPolicy]string{
		EvictLRU:    "allkeys-lru",
		EvictLFU:    "allkeys-lfu",
		EvictRandom: "allkeys-random",
	} {
		reconfigure(t, e, func(o *Options) { o.Eviction = policy })
		res, _ := Decode(e.cmdINFO([]string{"memory"}))
		assert.Contains(t, res.(string), "maxmemory_policy:"+want)
	}
}

func TestHumanBytes(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	e := newTestEngine(t, Options{})
	res, _ := Decode(e.cmdDBSIZE([]string{}))
	assert.EqualValues(t, 0, res)

	e.cmdSET([]string{"a", "1"})
	e.cmdSET([]string{"b", "2"})
	res, _ = Decode(e.cmdDBSIZE([]string{}))
	assert.EqualValues(t, 2, res)

	res, _ = Decode(e.cmdDBSIZE([]string{"extra"}))
	assert.Contains(t, res, "wrong number of arguments")
}
