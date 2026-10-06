package core

import (
	"testing"
)

// Tests run on engines of their own (plan step 2.6), so that they can run side
// by side: a test that calls t.Parallel makes its engine with newTestEngine
// and drives it through the helpers below, which take the engine they act on.
// The package's functions, and the helpers that call them, act on the default
// engine the server runs on, and a test that runs in parallel must not reach
// it; TestParallelTestsLeaveTheDefaultEngineAlone checks that none does.

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
// reply; run is runOn on the default engine. As with run, an error the
// command returns rather than answers - a command this server does not have -
// comes back as its text.
func runOn(t testing.TB, e *Engine, name string, args ...string) interface{} {
	t.Helper()
	var w replyWriter
	if err := e.evalAndResponse(&Command{Cmd: name, Args: args}, &w); err != nil {
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
	if err := e.evalAndResponse(&Command{Cmd: name, Args: args, RESP3: resp3}, &w); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return w.b
}
