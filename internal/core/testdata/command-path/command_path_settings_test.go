package core

import (
	"testing"

	"github.com/brandopakel/keel/internal/config"
)

// This file is not built with the package: the go tool skips testdata. It is
// command_path_settings_test.go as a baseline from before step 2.5 of the
// embedding plan needs it, and command-path.yml copies it into a baseline
// that lacks the log-on and replica-on benchmarks, beside the candidate's
// copies of them. It sets what those benchmarks run under through config,
// which such a baseline reads its settings from, and it leaves the key bound
// at that baseline's default of 5,000,000, which is the key cap the
// candidate's settings give its engine. A change to one is a change to both.

func logBenchmarkSettings(b *testing.B) {
	fsync, percentage := config.AOFFsync, config.AOFAutoRewritePercentage
	b.Cleanup(func() { config.AOFFsync, config.AOFAutoRewritePercentage = fsync, percentage })
	config.AOFFsync, config.AOFAutoRewritePercentage = config.FsyncEverySec, 0
}

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
