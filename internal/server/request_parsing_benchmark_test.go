package server

import (
	"strings"
	"testing"

	"github.com/brandopakel/keel/internal/core"
)

var requestParsingBenchmarkSink []*core.Command

// This fixture also compiles against the pre-admission runtime. The wire is
// already buffered, so it isolates read-phase ownership allocations from socket
// scheduling. It does not measure end-to-end throughput or the atomic budget.
func BenchmarkReadPhaseOwnership(b *testing.B) {
	for _, fixture := range []struct {
		name  string
		count int
	}{{"single", 1}, {"pipeline64", 64}} {
		b.Run(fixture.name, func(b *testing.B) {
			wire := []byte(strings.Repeat(string(encodeCmd("SET", "key", strings.Repeat("v", 64))), fixture.count))
			b.ReportAllocs()
			for b.Loop() {
				c := client{bufferedReady: true, buf: &connBuffer{data: wire}}
				cmds, err := c.readCommands(testScratch)
				if err != nil || len(cmds) != fixture.count || c.buf != nil {
					b.Fatal("read-phase ownership changed")
				}
				requestParsingBenchmarkSink = cmds
			}
		})
	}
}
