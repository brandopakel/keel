package core

import (
	"io"
	"testing"
)

// The engine the command-path benchmarks run on, and the settings
// BenchmarkCommandPathWithLog and BenchmarkCommandPathWithReplica run under,
// in one place.
//
// command-path.yml builds those two benchmarks' files into a baseline that
// predates them, such as 65ebdbc for the cumulative comparison, which has no
// Engine, no engine options and not this file. So the two files name no
// engine API: each family takes its engine from logBenchmarkEngine or
// replicaBenchmarkEngine, a benchEngine, and calls on it only EvalAndResponse
// below and Engine's OpenAOF, FlushAOF, CloseAOF, AOFStats and
// InitReplication, and mustSucceedOn. The job gives such a baseline
// testdata/command-path/command_path_settings_test.go in place of this file,
// whose benchEngine is that baseline's one keyspace: making one sets the same
// settings through config and empties the stores, and each of its methods
// calls the baseline's package function of the same name. A change to one is
// a change to both.

// benchEngine is the engine a command-path benchmark runs on. Its method is
// inlined, so a timed loop calls the engine's dispatch directly.
type benchEngine struct{ *Engine }

// EvalAndResponse runs cmd on the engine, as the package function of that
// name runs it on a baseline's one keyspace.
func (e benchEngine) EvalAndResponse(cmd *Command, w io.ReadWriter) error {
	return e.evalAndResponse(cmd, w)
}

// logBenchmarkEngine returns an engine of b's own held to what the log-on
// benchmark measures: the log under everysec, the server's default, with
// automatic rewrites off, and the server's former key cap, which every
// baseline from before step 2.5 holds its engine to.
func logBenchmarkEngine(b *testing.B) benchEngine {
	return benchEngine{newTestEngine(b, logBenchmarkOptions())}
}

// replicaBenchmarkEngine is logBenchmarkEngine's, as a protocol 2 primary
// that feeds a stream.
func replicaBenchmarkEngine(b *testing.B) benchEngine {
	o := logBenchmarkOptions()
	o.ReplicationFeed, o.ReplicationProtocol = true, 2
	return benchEngine{newTestEngine(b, o)}
}

// logBenchmarkOptions are the options the log-on benchmark's engine is held
// to.
func logBenchmarkOptions() Options {
	return Options{Fsync: FsyncEverySec, AutoRewritePercentage: Off, MaxKeys: serverKeyCap}
}

// mustSucceedOn runs cmd once on e and fails the benchmark if it errors, so a
// family whose command was removed or broke cannot report the cost of an
// error reply.
func mustSucceedOn(b *testing.B, e benchEngine, cmd *Command) {
	b.Helper()
	var w replyWriter
	if err := e.EvalAndResponse(cmd, &w); err != nil {
		b.Fatalf("%s: %v", cmd.Cmd, err)
	}
	if len(w.b) > 0 && w.b[0] == '-' {
		b.Fatalf("%s %v answered %q", cmd.Cmd, cmd.Args, w.b)
	}
}
