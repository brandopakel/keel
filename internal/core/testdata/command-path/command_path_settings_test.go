package core

import (
	"io"
	"testing"

	"github.com/brandopakel/keel/internal/config"
)

// This file is not built with the package: the go tool skips testdata. It is
// command_path_settings_test.go as a baseline from before engines needs it,
// and command-path.yml copies it into a baseline that lacks the log-on and
// replica-on benchmarks, beside the candidate's copies of them. Such a
// baseline, 65ebdbc for the cumulative comparison, has one keyspace, which its
// package functions act on, and reads its settings from config. So here a
// benchEngine is that keyspace: making one sets what the benchmarks run under
// through config, as the candidate's options do, and empties the stores, and
// each of its methods calls the baseline's package function of the same name.
// It leaves the key bound at such a baseline's default of 5,000,000, which is
// the key cap the candidate's settings give its engine. A change to one is a
// change to both.

// benchEngine is the baseline's one keyspace. Every method of it is inlined,
// so a timed loop calls the baseline's dispatch directly, as the candidate's
// calls its engine's.
type benchEngine struct{}

func (benchEngine) EvalAndResponse(cmd *Command, c io.ReadWriter) error {
	return EvalAndResponse(cmd, c)
}
func (benchEngine) OpenAOF(path string) error { return OpenAOF(path) }
func (benchEngine) FlushAOF() error           { return FlushAOF() }
func (benchEngine) CloseAOF() error           { return CloseAOF() }
func (benchEngine) InitReplication() error    { return InitReplication() }
func (benchEngine) AOFStats() (baseSize, currentSize int64, rewrites int, keys int) {
	return AOFStats()
}

func logBenchmarkEngine(b *testing.B) benchEngine {
	fsync, percentage := config.AOFFsync, config.AOFAutoRewritePercentage
	b.Cleanup(func() { config.AOFFsync, config.AOFAutoRewritePercentage = fsync, percentage })
	config.AOFFsync, config.AOFAutoRewritePercentage = config.FsyncEverySec, 0
	b.Cleanup(ResetStores)
	ResetStores()
	return benchEngine{}
}

func replicaBenchmarkEngine(b *testing.B) benchEngine {
	b.Cleanup(func() {
		if err := InitReplication(); err != nil {
			b.Error(err)
		}
	})
	e := logBenchmarkEngine(b)
	feed, protocol, replicaOf := config.ReplicationFeed, config.ReplicationProtocol, config.ReplicaOf
	b.Cleanup(func() {
		config.ReplicationFeed, config.ReplicationProtocol, config.ReplicaOf = feed, protocol, replicaOf
	})
	config.ReplicationFeed, config.ReplicationProtocol, config.ReplicaOf = true, 2, ""
	return e
}

func mustSucceedOn(b *testing.B, _ benchEngine, cmd *Command) {
	b.Helper()
	mustSucceed(b, cmd)
}
