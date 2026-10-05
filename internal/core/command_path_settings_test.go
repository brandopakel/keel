package core

import (
	"testing"

	"github.com/brandopakel/keel/internal/config"
)

// The settings BenchmarkCommandPathWithLog and BenchmarkCommandPathWithReplica
// run under, set in one place.
//
// command-path.yml builds those benchmarks' files into a baseline that
// predates them, such as 65ebdbc for the cumulative comparison, which has
// neither the engine options these set nor this file. So the benchmarks set
// nothing themselves: they call these, and the job gives such a baseline
// testdata/command-path/command_path_settings_test.go in place of this file,
// which sets the same through the config variables that baseline has. A
// change to one is a change to both.

// logBenchmarkSettings holds the default engine to what the log-on benchmark
// measures: the log under everysec, the server's default, with automatic
// rewrites off, and the server's key cap, which every baseline holds its
// engine to. What it found is put back when b ends.
func logBenchmarkSettings(b *testing.B) {
	fsync, percentage := config.AOFFsync, config.AOFAutoRewritePercentage
	b.Cleanup(func() { config.AOFFsync, config.AOFAutoRewritePercentage = fsync, percentage })
	config.AOFFsync, config.AOFAutoRewritePercentage = config.FsyncEverySec, 0
	withOptions(b, func(o *Options) { o.MaxKeys = serverKeyCap })
}

// replicaBenchmarkSettings is logBenchmarkSettings with the engine a protocol
// 2 primary that feeds a stream. When b ends, the role it found is put back
// and the replication state started again for it.
func replicaBenchmarkSettings(b *testing.B) {
	b.Cleanup(func() {
		if err := InitReplication(); err != nil {
			b.Error(err)
		}
	})
	logBenchmarkSettings(b)
	feed, protocol, replicaOf := config.ReplicationFeed, config.ReplicationProtocol, config.ReplicaOf
	b.Cleanup(func() {
		config.ReplicationFeed, config.ReplicationProtocol, config.ReplicaOf = feed, protocol, replicaOf
	})
	config.ReplicationFeed, config.ReplicationProtocol, config.ReplicaOf = true, 2, ""
}
