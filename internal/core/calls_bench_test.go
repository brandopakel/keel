package core

import (
	"context"
	"path/filepath"
	"testing"
)

// BenchmarkEmbeddedCall measures what a call costs an embedded caller over
// the command it runs (plan phase 3): the lock, the checks, and the wait for
// the published offset. "direct" is the command as the server's loop runs it,
// under no lock of its own; the rest are calls on an engine Open made, with
// no log, and with a log under everysec and under always, from one goroutine
// and from several. It is reported, not gated: there is nothing before phase
// 3 to pair it with, and its name keeps it out of the paired command-path
// job's ^BenchmarkCommandPath.
func BenchmarkEmbeddedCall(b *testing.B) {
	set := &Command{Cmd: "SET", Args: []string{"key", "value"}}
	b.Run("direct", func(b *testing.B) {
		e := newTestEngine(b, Options{})
		b.ReportAllocs()
		for b.Loop() {
			if err := e.EvalAndResponse(set, discardWriter{}); err != nil {
				b.Fatal(err)
			}
		}
	})
	for _, c := range []struct {
		name string
		o    func(dir string) Options
	}{
		{"no log", func(string) Options { return Options{} }},
		{"everysec", func(dir string) Options {
			return Options{AppendOnly: true, AppendFilename: filepath.Join(dir, "keel.aof"), Fsync: FsyncEverySec}
		}},
		{"always", func(dir string) Options {
			return Options{AppendOnly: true, AppendFilename: filepath.Join(dir, "keel.aof"), Fsync: FsyncAlways}
		}},
	} {
		b.Run(c.name, func(b *testing.B) {
			e, err := Open(context.Background(), c.o(b.TempDir()))
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = e.Close() })
			ctx := context.Background()
			b.ReportAllocs()
			for b.Loop() {
				if err := e.Do(ctx, set, discardWriter{}); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(c.name+", parallel", func(b *testing.B) {
			e, err := Open(context.Background(), c.o(b.TempDir()))
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = e.Close() })
			ctx := context.Background()
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					if err := e.Do(ctx, set, discardWriter{}); err != nil {
						b.Error(err)
						return
					}
				}
			})
		})
	}
}
