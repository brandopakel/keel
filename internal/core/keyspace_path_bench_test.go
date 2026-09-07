package core

import (
	"strconv"
	"testing"
)

// The sharded keyspace adds a hash of the key name to every store operation.
// That delta is measured against a plain map in data_structure; this measures
// the whole command it sits inside, so the two can be read as a share rather
// than as a bare nanosecond count.
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
}

func run2(b *testing.B, name string, args ...string) {
	b.Helper()
	var w replyWriter
	if err := EvalAndResponse(&Command{Cmd: name, Args: args}, &w); err != nil {
		b.Fatalf("%s: %v", name, err)
	}
}
