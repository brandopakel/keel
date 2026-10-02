package core

import (
	"strconv"
	"testing"

	"github.com/brandopakel/keel/internal/config"
)

// The sharded keyspace adds a hash of the key name to every store operation.
// That delta is measured against a plain map in data_structure; this measures
// the whole command it sits inside, so the two can be read as a share rather
// than as a bare nanosecond count.
//
// The families beyond SET and GET are the baseline the embedding plan
// (docs/embedding-plan.md) is measured against: every step of moving the
// stores into an engine adds an indirection to exactly this path, and the
// paired job in command-path.yml compares each change with its base here.
// Sub-benchmark names are part of that comparison, so a name, once added, is
// not renamed.
func BenchmarkCommandPath(b *testing.B) {
	ResetStores()
	keys := make([]string, 1000)
	for i := range keys {
		keys[i] = "bench:key:" + strconv.Itoa(i)
		run2(b, "SET", keys[i], "value")
	}

	b.Run("SET", func(b *testing.B) {
		var w replyWriter
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			w.b = w.b[:0]
			EvalAndResponse(&Command{Cmd: "SET", Args: []string{keys[i%len(keys)], "value"}}, &w)
		}
	})
	b.Run("GET", func(b *testing.B) {
		var w replyWriter
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			w.b = w.b[:0]
			EvalAndResponse(&Command{Cmd: "GET", Args: []string{keys[i%len(keys)]}}, &w)
		}
	})

	// Each family below runs a fixed ring of prepared commands, so what is
	// counted - time and allocations - is the command, not building it.
	members := make([]string, 100)
	for i := range members {
		members[i] = "member:" + strconv.Itoa(i)
	}
	for i, m := range members {
		run2(b, "HSET", "bench:hash", m, "value")
		run2(b, "RPUSH", "bench:list", m)
		run2(b, "SADD", "bench:set", m)
		run2(b, "ZADD", "bench:zset", strconv.Itoa(i), m)
	}
	ring := func(n int, build func(i int) *Command) []*Command {
		cmds := make([]*Command, n)
		for i := range cmds {
			cmds[i] = build(i)
		}
		return cmds
	}
	key := func(i int) string { return keys[i%len(keys)] }
	member := func(i int) string { return members[i%len(members)] }
	for _, family := range []struct {
		name string
		cmds []*Command
	}{
		{"SET-EX", ring(1000, func(i int) *Command { return &Command{Cmd: "SET", Args: []string{key(i), "value", "EX", "3600"}} })},
		{"INCR", ring(1000, func(i int) *Command { return &Command{Cmd: "INCR", Args: []string{"bench:counter:" + strconv.Itoa(i)}} })},
		{"MGET-10", ring(100, func(i int) *Command {
			return &Command{Cmd: "MGET", Args: []string{key(i), key(i + 1), key(i + 2), key(i + 3), key(i + 4), key(i + 5), key(i + 6), key(i + 7), key(i + 8), key(i + 9)}}
		})},
		{"HSET", ring(100, func(i int) *Command { return &Command{Cmd: "HSET", Args: []string{"bench:hash", member(i), "value"}} })},
		{"HGET", ring(100, func(i int) *Command { return &Command{Cmd: "HGET", Args: []string{"bench:hash", member(i)}} })},
		{"HGETALL-100", ring(1, func(int) *Command { return &Command{Cmd: "HGETALL", Args: []string{"bench:hash"}} })},
		{"LRANGE-100", ring(1, func(int) *Command { return &Command{Cmd: "LRANGE", Args: []string{"bench:list", "0", "-1"}} })},
		{"SADD", ring(100, func(i int) *Command { return &Command{Cmd: "SADD", Args: []string{"bench:set", member(i)}} })},
		{"SISMEMBER", ring(100, func(i int) *Command { return &Command{Cmd: "SISMEMBER", Args: []string{"bench:set", member(i)}} })},
		{"ZADD", ring(100, func(i int) *Command {
			return &Command{Cmd: "ZADD", Args: []string{"bench:zset", strconv.Itoa(i * 7), member(i)}}
		})},
		{"ZSCORE", ring(100, func(i int) *Command { return &Command{Cmd: "ZSCORE", Args: []string{"bench:zset", member(i)}} })},
		{"ZRANGE-100-WITHSCORES", ring(1, func(int) *Command {
			return &Command{Cmd: "ZRANGE", Args: []string{"bench:zset", "0", "-1", "WITHSCORES"}}
		})},
		{"PFADD", ring(1000, func(i int) *Command { return &Command{Cmd: "PFADD", Args: []string{"bench:hll", strconv.Itoa(i)}} })},
		{"BF.ADD", ring(1000, func(i int) *Command { return &Command{Cmd: "BF.ADD", Args: []string{"bench:bloom", strconv.Itoa(i)}} })},
	} {
		b.Run(family.name, func(b *testing.B) {
			var w replyWriter
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				w.b = w.b[:0]
				EvalAndResponse(family.cmds[i%len(family.cmds)], &w)
			}
		})
	}

	// A list held at a constant length: one push and one pop per iteration.
	b.Run("LPUSH-RPOP", func(b *testing.B) {
		var w replyWriter
		push := &Command{Cmd: "LPUSH", Args: []string{"bench:list", "value"}}
		pop := &Command{Cmd: "RPOP", Args: []string{"bench:list"}}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			w.b = w.b[:0]
			EvalAndResponse(push, &w)
			EvalAndResponse(pop, &w)
		}
	})
}

// BenchmarkCommandPathUnderEviction writes new keys into a full budget, so every
// SET also runs the evictor: the shared sample pool, the clock and a removal
// from whichever keyspace the victim lives in. That cross-keyspace path is the
// one an engine-owned Space must not slow down.
func BenchmarkCommandPathUnderEviction(b *testing.B) {
	for _, policy := range []struct {
		name     string
		strategy int
	}{{"random", config.EvictFirst}, {"lru", config.LRU}, {"lfu", config.LFU}} {
		b.Run(policy.name, func(b *testing.B) {
			oldMax, oldStrategy := config.MaxMemory, config.EvictStrategy
			b.Cleanup(func() { config.MaxMemory, config.EvictStrategy = oldMax, oldStrategy })
			ResetStores()
			config.MaxMemory, config.EvictStrategy = 4<<20, policy.strategy
			value := string(make([]byte, 256))
			cmds := make([]*Command, 1<<16)
			for i := range cmds {
				cmds[i] = &Command{Cmd: "SET", Args: []string{"bench:evict:" + strconv.Itoa(i), value}}
			}
			var w replyWriter
			for _, cmd := range cmds {
				w.b = w.b[:0]
				EvalAndResponse(cmd, &w)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				w.b = w.b[:0]
				// Cycling through more names than the budget holds keeps
				// every write a new key and every write an eviction.
				EvalAndResponse(cmds[i%len(cmds)], &w)
			}
		})
	}
}

func run2(b *testing.B, name string, args ...string) {
	b.Helper()
	var w replyWriter
	if err := EvalAndResponse(&Command{Cmd: name, Args: args}, &w); err != nil {
		b.Fatalf("%s: %v", name, err)
	}
}
