package core

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/brandopakel/keel/internal/config"
)

// BenchmarkCommandPathWithReplica is the command path of a protocol 2 primary
// with the log open: every write stages its record, appends it to the log's
// buffer and publishes it to the replication stream, which step 2.4 of the
// embedding plan (docs/embedding-plan.md) moves into the engine. It is
// BenchmarkCommandPathWithLog's families and cycle with the feed on.
//
// A replica is attached before the timer starts: it pulls the stream as a
// replica does, through KEEL.REPL.PULL2 in-process, and is level with the
// primary. The timed loop has no replica pull in it. A primary publishes
// every write the same way whether a replica is pulling or not, and a pull
// costs per pull, not per write: one JSON frame of up to 256 KiB, checksummed,
// a cost the step does not touch and whose size would drown the per-write
// cost the step does. After the timer stops, the stream must have grown by
// every write measured, in the same epoch, and a pull at its end must be
// served as a delta.
//
// No network is involved: the pull is a command like any other, run where the
// event loop would run it.
//
// This file uses only what the package had before step 2.4, and what
// command_path_log_bench_test.go uses, so command-path.yml can build it into
// a baseline that does not have it yet. Sub-benchmark names are part of that
// comparison, so a name, once added, is not renamed.
func BenchmarkCommandPathWithReplica(b *testing.B) {
	fsync, percentage := config.AOFFsync, config.AOFAutoRewritePercentage
	feed, protocol, replicaOf := config.ReplicationFeed, config.ReplicationProtocol, config.ReplicaOf
	b.Cleanup(func() {
		config.AOFFsync, config.AOFAutoRewritePercentage = fsync, percentage
		config.ReplicationFeed, config.ReplicationProtocol, config.ReplicaOf = feed, protocol, replicaOf
		if err := InitReplication(); err != nil {
			b.Error(err)
		}
		ResetStores()
	})
	config.AOFFsync, config.AOFAutoRewritePercentage = config.FsyncEverySec, 0
	config.ReplicationFeed, config.ReplicationProtocol, config.ReplicaOf = true, 2, ""

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
		name  string
		setup []*Command
		steps [][]*Command
	}{
		{"SET", nil, ring(1000, func(i int) []*Command { return one("SET", "bench:key:"+strconv.Itoa(i), "value") })},
		{"INCR", nil, ring(1000, func(i int) []*Command { return one("INCR", "bench:counter:"+strconv.Itoa(i)) })},
		{"HSET", nil, ring(100, func(i int) []*Command { return one("HSET", "bench:hash", members[i], "value") })},
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
			if err := InitReplication(); err != nil {
				b.Fatal(err)
			}
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
			epoch, before := replicaAttach(b)
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
			// Every write this measured reached the stream, in the epoch the
			// replica attached in: no record is shorter than minRecordBytes.
			// The history keeps the last 16 MiB, so the replica, had it kept
			// pulling, is level again after one pull at the end.
			after, end := replicationPosition(b)
			if end-before < uint64(b.N)*minRecordBytes {
				b.Fatalf("the stream grew %d bytes over %d iterations", end-before, b.N)
			}
			if after != epoch {
				b.Fatalf("the stream changed epoch: %s, then %s", epoch, after)
			}
			replicaPull(b, epoch, end)
		})
	}
}

// replicaAttach pulls the stream as a replica that is level with the primary
// does, and returns the epoch and the offset it is at.
func replicaAttach(b *testing.B) (string, uint64) {
	b.Helper()
	epoch, end := replicationPosition(b)
	if got, _ := replicaPull(b, epoch, end); got != epoch {
		b.Fatalf("attached to epoch %s, not %s", got, epoch)
	}
	return epoch, end
}

// replicaPull is one protocol 2 delta pull from offset, through the command
// path, which must be served as a delta; it returns the stream's epoch and
// end afterwards.
func replicaPull(b *testing.B, epoch string, offset uint64) (string, uint64) {
	b.Helper()
	var w replyWriter
	cmd := &Command{Cmd: "KEEL.REPL.PULL2", Args: []string{epoch, strconv.FormatUint(offset, 10), "", "0"}}
	if err := EvalAndResponse(cmd, &w); err != nil {
		b.Fatal(err)
	}
	reply, err := Decode(w.b)
	encoded, ok := reply.(string)
	var frame ReplicationFrame
	if err == nil && ok {
		err = json.Unmarshal([]byte(encoded), &frame)
	}
	if err != nil || !ok || frame.Full || frame.Epoch != epoch {
		b.Fatalf("KEEL.REPL.PULL2 %s %d: %v %q", epoch, offset, err, w.b)
	}
	return replicationPosition(b)
}

// replicationPosition reads the primary's epoch and stream end from INFO.
func replicationPosition(b *testing.B) (string, uint64) {
	b.Helper()
	var w replyWriter
	if err := EvalAndResponse(&Command{Cmd: "INFO", Args: []string{"replication"}}, &w); err != nil {
		b.Fatal(err)
	}
	var epoch string
	var end uint64
	for _, line := range strings.Split(string(w.b), "\r\n") {
		if v, ok := strings.CutPrefix(line, "primary_epoch:"); ok {
			epoch = v
		} else if v, ok := strings.CutPrefix(line, "primary_offset:"); ok {
			n, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				b.Fatal(err)
			}
			end = n
		}
	}
	if len(epoch) != 32 {
		b.Fatalf("INFO replication has no epoch: %q", w.b)
	}
	return epoch, end
}
