package data_structure

import (
	"runtime"
	"strconv"
	"testing"
)

// The sharded keyspace is only worth having if the cursor it buys costs little
// on the paths every command uses. These compare it against the plain Go map it
// replaced, in the same binary and back to back, so a loaded machine moves both
// arms together rather than favouring whichever ran first.
//
// persistence-replication-next.md asks for per-key memory and mutation overhead
// at 100k and 1M keys before adopting a traversal abstraction. That is what
// these measure.

func benchKeys(n int) []string {
	keys := make([]string, n)
	for i := range keys {
		keys[i] = "keyspace:entry:" + strconv.Itoa(i)
	}
	return keys
}

func BenchmarkPlainMapGet(b *testing.B) {
	for _, n := range []int{100000, 1000000} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			keys := benchKeys(n)
			m := make(map[string]int, n)
			for i, k := range keys {
				m[k] = i
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = m[keys[i%n]]
			}
		})
	}
}

func BenchmarkShardedGet(b *testing.B) {
	for _, n := range []int{100000, 1000000} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			keys := benchKeys(n)
			m := &shardedMap[int]{}
			for i, k := range keys {
				m.set(k, i)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, _ = m.get(keys[i%n])
			}
		})
	}
}

func BenchmarkPlainMapSet(b *testing.B) {
	for _, n := range []int{100000, 1000000} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			keys := benchKeys(n)
			m := make(map[string]int, n)
			for i, k := range keys {
				m[k] = i
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				m[keys[i%n]] = i
			}
		})
	}
}

func BenchmarkShardedSet(b *testing.B) {
	for _, n := range []int{100000, 1000000} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			keys := benchKeys(n)
			m := &shardedMap[int]{}
			for i, k := range keys {
				m.set(k, i)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				m.set(keys[i%n], i)
			}
		})
	}
}

// Keys is what a rewrite and KEYS still pay: the whole slice at once. The point
// of comparing it is that sharding must not make the existing O(N) walk worse
// while it adds a bounded one beside it.
func BenchmarkPlainMapKeys(b *testing.B) {
	for _, n := range []int{100000, 1000000} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			keys := benchKeys(n)
			m := make(map[string]int, n)
			for i, k := range keys {
				m[k] = i
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				out := make([]string, 0, len(m))
				for k := range m {
					out = append(out, k)
				}
				_ = out
			}
		})
	}
}

func BenchmarkShardedKeys(b *testing.B) {
	for _, n := range []int{100000, 1000000} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			keys := benchKeys(n)
			m := &shardedMap[int]{}
			for i, k := range keys {
				m.set(k, i)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = m.keys()
			}
		})
	}
}

// One SCAN call, which is the whole point: bounded work regardless of how large
// the keyspace is. Compare its cost against the full walk above.
func BenchmarkShardedScanOneCall(b *testing.B) {
	for _, n := range []int{100000, 1000000} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			keys := benchKeys(n)
			m := &shardedMap[int]{}
			for i, k := range keys {
				m.set(k, i)
			}
			dst := make([]string, 0, 4096)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, _, _ = m.scan(uint64(i%n), 10, nil, dst[:0])
			}
		})
	}
}

// TestShardedMemoryOverhead reports what the partition costs in bytes per key
// against the same keys in one map. It asserts only a loose ceiling: the number
// to read is the logged one, and it is a measurement, not a threshold anyone
// should tune against.
func TestShardedMemoryOverhead(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates a million keys")
	}
	for _, n := range []int{100000, 1000000} {
		keys := benchKeys(n)

		plain := heldBytes(func() any {
			m := make(map[string]int)
			for i, k := range keys {
				m[k] = i
			}
			return m
		})
		sharded := heldBytes(func() any {
			m := &shardedMap[int]{}
			for i, k := range keys {
				m.set(k, i)
			}
			return m
		})

		// The bare int fixture pays an inline key/value slot. Real stores
		// offset that cost by eliminating their separately allocated entry.
		// The real-dictionary heap tests remain the adoption gate.
		perKey := float64(int64(sharded)-int64(plain)) / float64(n)
		t.Logf("%d keys: plain %.2f MiB, sharded %.2f MiB, %+.1f bytes per key",
			n, float64(plain)/(1<<20), float64(sharded)/(1<<20), perKey)
		if perKey > 16 {
			t.Errorf("%d keys: %.1f bytes per key of slot-directory overhead is more than expected", n, perKey)
		}
	}
}

// heldBytes measures the live heap attributable to what build returns, by
// comparing a settled heap before and after and keeping the value alive across
// the second reading.
func heldBytes(build func() any) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	held := build()
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(held)
	if after.HeapAlloc < before.HeapAlloc {
		return 0
	}
	return after.HeapAlloc - before.HeapAlloc
}
