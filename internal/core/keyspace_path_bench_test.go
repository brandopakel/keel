package core

import (
	"strconv"
	"testing"
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
//
// It runs on an engine of its own, held to the server's former key cap, where
// a baseline from before step 2.7 resets its default engine's stores. Its
// sub-benchmarks share that engine and the keys set up here, as they shared
// the default engine.
func BenchmarkCommandPath(b *testing.B) {
	e := benchEngine{newTestEngine(b, Options{MaxKeys: serverKeyCap})}
	keys := make([]string, 1000)
	for i := range keys {
		keys[i] = "bench:key:" + strconv.Itoa(i)
		run2On(b, e, "SET", keys[i], "value")
	}

	b.Run("SET", func(b *testing.B) {
		var w replyWriter
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			w.b = w.b[:0]
			e.EvalAndResponse(&Command{Cmd: "SET", Args: []string{keys[i%len(keys)], "value"}}, &w)
		}
	})
	b.Run("GET", func(b *testing.B) {
		var w replyWriter
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			w.b = w.b[:0]
			e.EvalAndResponse(&Command{Cmd: "GET", Args: []string{keys[i%len(keys)]}}, &w)
		}
	})

	// Each family below runs a fixed ring of prepared commands, so what is
	// counted - time and allocations - is the command, not building it.
	members := make([]string, 100)
	for i := range members {
		members[i] = "member:" + strconv.Itoa(i)
	}
	for i, m := range members {
		run2On(b, e, "HSET", "bench:hash", m, "value")
		run2On(b, e, "RPUSH", "bench:list", m)
		run2On(b, e, "SADD", "bench:set", m)
		run2On(b, e, "ZADD", "bench:zset", strconv.Itoa(i), m)
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
			for _, cmd := range family.cmds {
				mustSucceedOn(b, e, cmd)
			}
			var w replyWriter
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				w.b = w.b[:0]
				e.EvalAndResponse(family.cmds[i%len(family.cmds)], &w)
			}
		})
	}

	// A list held at a constant length: one push and one pop per iteration.
	b.Run("LPUSH-RPOP", func(b *testing.B) {
		var w replyWriter
		push := &Command{Cmd: "LPUSH", Args: []string{"bench:list", "value"}}
		pop := &Command{Cmd: "RPOP", Args: []string{"bench:list"}}
		mustSucceedOn(b, e, push)
		mustSucceedOn(b, e, pop)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			w.b = w.b[:0]
			e.EvalAndResponse(push, &w)
			e.EvalAndResponse(pop, &w)
		}
	})
}

// BenchmarkCommandPathUnderEviction writes new keys into a full budget, so every
// SET also runs the evictor: the shared sample pool, the clock and a removal
// from whichever keyspace the victim lives in. That cross-keyspace path is the
// one an engine-owned Space must not slow down.
//
// Each policy runs on an engine of its own, held to the budget, the policy and
// the server's former key cap, where a baseline from before step 2.7 resets
// its default engine's stores and sets the same.
func BenchmarkCommandPathUnderEviction(b *testing.B) {
	for _, policy := range []struct {
		name     string
		strategy EvictionPolicy
	}{{"random", EvictRandom}, {"lru", EvictLRU}, {"lfu", EvictLFU}} {
		b.Run(policy.name, func(b *testing.B) {
			e := benchEngine{newTestEngine(b, Options{MaxMemory: 4 << 20, MaxKeys: serverKeyCap, Eviction: policy.strategy})}
			value := string(make([]byte, 256))
			cmds := make([]*Command, 1<<16)
			for i := range cmds {
				cmds[i] = &Command{Cmd: "SET", Args: []string{"bench:evict:" + strconv.Itoa(i), value}}
			}
			for _, cmd := range cmds {
				mustSucceedOn(b, e, cmd)
			}
			var w replyWriter
			before := e.space.Evicted()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				w.b = w.b[:0]
				// Cycling through four times the names the budget holds keeps
				// nearly every write a new key, and so an eviction.
				e.EvalAndResponse(cmds[i%len(cmds)], &w)
			}
			b.StopTimer()
			// A result for this benchmark is only about eviction if the timed
			// writes evicted; one that stopped would otherwise just look fast.
			evicted := e.space.Evicted() - before
			b.ReportMetric(float64(evicted)/float64(b.N), "evictions/op")
			if evicted < uint64(b.N/2) {
				b.Fatalf("%d evictions over %d writes: the timed path is not the eviction path", evicted, b.N)
			}
		})
	}
}

// serverKeyCap is the key bound the server's -maxkeys flag defaulted to until
// 2026-10-05, when the server took Redis's unbounded default. Every build
// before step 2.5 of the plan held its engine to it, whatever ran on it; since
// then an engine has no key bound unless it is given one. The benchmarks
// still give their engines this one, so that every write counts the keyspace
// as a baseline from before step 2.5 counts it, and the paired job keeps
// comparing the same work.
const serverKeyCap = 5000000

// run2On runs a command on e for a benchmark's setup, which must succeed.
func run2On(b *testing.B, e benchEngine, name string, args ...string) {
	b.Helper()
	mustSucceedOn(b, e, &Command{Cmd: name, Args: args})
}
