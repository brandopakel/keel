package core

import (
	"path/filepath"
	"strconv"
	"testing"
)

// logCycle is how many commands BenchmarkCommandPathWithLog runs between
// flushes: one event-loop cycle of pipelined commands.
const logCycle = 64

// minRecordBytes is less than any record a family's iteration logs, so the
// log's growth shows each iteration's writes reached it.
const minRecordBytes = 16

// BenchmarkCommandPathWithLog is the command path with the log open. Every
// write stages its record and appends it to the log's buffer, and step 2.3 of
// the embedding plan (docs/embedding-plan.md) moves that state into the
// engine, so the paired job in command-path.yml measures it here as well as
// without a log in BenchmarkCommandPath.
//
// The log is under everysec, the server's default, in a temporary directory,
// with automatic rewrites off (logBenchmarkSettings). The flush runs once
// every logCycle commands, as the event loop runs it once a cycle, so its
// write is in the time per command at the share a pipelined cycle pays.
//
// This file uses only what the package had before step 2.3, and the settings
// in command_path_settings_test.go, so command-path.yml can build it into a
// baseline that does not have it yet. Sub-benchmark names are part of that
// comparison, so a name, once added, is not renamed.
func BenchmarkCommandPathWithLog(b *testing.B) {
	b.Cleanup(ResetStores)
	logBenchmarkSettings(b)

	members := make([]string, 100)
	for i := range members {
		members[i] = "member:" + strconv.Itoa(i)
	}
	ring := func(n int, build func(i int) []*Command) [][]*Command {
		cmds := make([][]*Command, n)
		for i := range cmds {
			cmds[i] = build(i)
		}
		return cmds
	}
	one := func(name string, args ...string) []*Command { return []*Command{{Cmd: name, Args: args}} }
	for _, family := range []struct {
		name string
		// Each step is one iteration: a command, or LPUSH then RPOP. Each
		// runs once before the timer starts, after setup.
		setup []*Command
		steps [][]*Command
	}{
		{"SET", nil, ring(1000, func(i int) []*Command { return one("SET", "bench:key:"+strconv.Itoa(i), "value") })},
		{"INCR", nil, ring(1000, func(i int) []*Command { return one("INCR", "bench:counter:"+strconv.Itoa(i)) })},
		{"HSET", nil, ring(100, func(i int) []*Command { return one("HSET", "bench:hash", members[i], "value") })},
		// A list held at a constant length of a hundred, as in
		// BenchmarkCommandPath: one push and one pop per iteration.
		{"LPUSH-RPOP", one("RPUSH", append([]string{"bench:list"}, members...)...), ring(1, func(int) []*Command {
			return append(one("LPUSH", "bench:list", "value"), one("RPOP", "bench:list")...)
		})},
		{"SADD", nil, ring(100, func(i int) []*Command { return one("SADD", "bench:set", members[i]) })},
	} {
		b.Run(family.name, func(b *testing.B) {
			ResetStores()
			if err := OpenAOF(filepath.Join(b.TempDir(), "bench.aof")); err != nil {
				b.Fatal(err)
			}
			defer CloseAOF()
			for _, cmd := range family.setup {
				mustSucceed(b, cmd)
			}
			for _, step := range family.steps {
				for _, cmd := range step {
					mustSucceed(b, cmd)
				}
			}
			if err := FlushAOF(); err != nil {
				b.Fatal(err)
			}
			_, before, _, _ := AOFStats()
			var w replyWriter
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for _, cmd := range family.steps[i%len(family.steps)] {
					w.b = w.b[:0]
					if err := EvalAndResponse(cmd, &w); err != nil || len(w.b) == 0 || w.b[0] == '-' {
						b.Fatalf("%s %v: %v %q", cmd.Cmd, cmd.Args, err, w.b)
					}
				}
				if i%logCycle == logCycle-1 {
					if err := FlushAOF(); err != nil {
						b.Fatal(err)
					}
				}
			}
			b.StopTimer()
			if err := FlushAOF(); err != nil {
				b.Fatal(err)
			}
			// Every write this measured is in the log: no record is shorter
			// than minRecordBytes.
			if _, after, _, _ := AOFStats(); after-before < int64(b.N)*minRecordBytes {
				b.Fatalf("the log grew %d bytes over %d iterations", after-before, b.N)
			}
		})
	}
}
