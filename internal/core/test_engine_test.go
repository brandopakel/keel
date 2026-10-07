package core

import (
	"testing"
)

// Tests run on engines of their own (plan step 2.6), so that they can run side
// by side: a test makes its engine with newTestEngine and drives it through
// the helpers below, which take the engine they act on. There is no other
// engine since step 2.7; TestParallelTestsShareNoPackageState checks that a
// test that runs in parallel writes none of the package's state either.

// newEngine is NewEngine for a test, whose options are a mistake in the test
// when no engine can be held to them, and panic.
func newEngine(o Options) *Engine {
	e, err := NewEngine(o)
	if err != nil {
		panic(err)
	}
	return e
}

// newTestEngine returns an engine of t's own, held to o, with empty stores in
// a space of its own, and closes it when t ends: its log, with the append
// worker, the sync and any rewrite in progress, and its replication snapshot.
//
// It takes t's temporary directory before it registers that cleanup, because
// cleanups run last first: the directory a test keeps the engine's log, term
// file or checkpoint in is removed only once the engine has let go of it,
// whenever the test asks for it.
func newTestEngine(t testing.TB, o Options) *Engine {
	t.Helper()
	t.TempDir()
	e := newEngine(o)
	t.Cleanup(func() {
		// A test that failed the engine's disk on purpose has checked what
		// it wanted; closing is for the descriptors and the workers.
		_ = e.CloseAOF()
	})
	return e
}

// runOn executes a command on e the way a connection would, and decodes the
// reply. An error the command returns rather than answers - a command this
// server does not have - comes back as its text.
//
// Calling cmdSET and friends directly skips EvalAndResponse, and that is
// where the log is written from - so a test that called them directly would
// drive the keyspace correctly and record none of it, then pass by observing
// that the keyspace was correct. Every command here goes the long way round
// for that reason.
func runOn(t testing.TB, e *Engine, name string, args ...string) interface{} {
	t.Helper()
	var w replyWriter
	if err := e.EvalAndResponse(&Command{Cmd: name, Args: args}, &w); err != nil {
		return err.Error()
	}
	res, _ := Decode(w.b)
	return res
}

// rawReplyOn returns the encoded reply of a command run on e, for the cases
// where nil and empty differ.
func rawReplyOn(t testing.TB, e *Engine, name string, args ...string) []byte {
	t.Helper()
	return rawReplyAsOn(t, e, false, name, args...)
}

// rawReplyAsOn runs a command on e the way a connection that negotiated the
// given protocol would, and returns the reply as written.
func rawReplyAsOn(t testing.TB, e *Engine, resp3 bool, name string, args ...string) []byte {
	t.Helper()
	var w replyWriter
	if err := e.EvalAndResponse(&Command{Cmd: name, Args: args, RESP3: resp3}, &w); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return w.b
}
