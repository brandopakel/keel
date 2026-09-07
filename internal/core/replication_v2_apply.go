package core

import (
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/brandopakel/keel/internal/config"
)

const maxReplicationSnapshotBytes = 1 << 30

var replicaV2 struct {
	trusted                                       bool
	resumed                                       bool
	snapshotID                                    string
	snapshotBytes, snapshotReceived, snapshotBase uint64
	pending                                       []byte
	checkpoint                                    replicaCheckpoint
}

func resetReplicaV2() {
	replicaV2.trusted = false
	replicaV2.resumed = false
	replicaV2.snapshotID = ""
	replicaV2.snapshotBytes = 0
	replicaV2.snapshotReceived = 0
	replicaV2.snapshotBase = 0
	replicaV2.pending = nil
	replicaV2.checkpoint = replicaCheckpoint{}
}

func applyReplicationV2(frame ReplicationFrame) (err error) {
	if AppendPending() {
		return errors.New("replication apply waits for pending append")
	}
	defer func() {
		if err != nil {
			replicaReady = false
			replicaV2.trusted = false
		}
	}()
	if frame.Version != 2 || len(frame.Epoch) != 32 || len(frame.Body) > replicationChunkBytes || frame.Checksum != frameChecksum(frame) {
		return errors.New("invalid protocol 2 frame")
	}
	// A frame from below the term this replica already knows is a deposed
	// primary still talking. Following it would rewind the replica onto a
	// history the cluster has abandoned.
	if frame.Term < failover.term {
		return fmt.Errorf("replication frame from term %d, below the known term %d", frame.Term, failover.term)
	}
	if err := observeTerm(frame.Term); err != nil {
		return err
	}
	if _, err := hex.DecodeString(frame.Epoch); err != nil {
		return err
	}
	if frame.Pending {
		if !frame.Full || len(frame.Body) != 0 {
			return errors.New("invalid pending snapshot frame")
		}
		replicaReady = false
		resetReplicaV2()
		replicaEpoch = ""
		replicaOffset = 0
		return nil
	}
	if frame.Full {
		if frame.SnapshotID == "" || len(frame.SnapshotID) != 32 || frame.SnapshotBytes > maxReplicationSnapshotBytes || frame.SnapshotOffset > frame.SnapshotBytes || uint64(len(frame.Body)) > frame.SnapshotBytes-frame.SnapshotOffset || frame.SnapshotDone != (frame.SnapshotOffset+uint64(len(frame.Body)) == frame.SnapshotBytes) {
			return errors.New("invalid snapshot chunk bounds")
		}
		if _, err := hex.DecodeString(frame.SnapshotID); err != nil {
			return err
		}
		if frame.SnapshotOffset == 0 {
			if frame.From != replicaOffset {
				return errors.New("snapshot does not match requested offset")
			}
			replicaReady = false
			resetReplicaV2()
			replicaV2.snapshotID = frame.SnapshotID
			replicaV2.snapshotBytes = frame.SnapshotBytes
			replicaV2.snapshotBase = frame.To
			replicaEpoch = frame.Epoch
			// The snapshot is an AOF prefix, not an independently readable frame.
			// Gate the old state before reset and keep reads gated until catch-up.
			replicaApplying = true
			var reply replicationReply
			err := EvalAndResponse(&Command{Cmd: "FLUSHDB"}, &reply)
			replicaApplying = false
			if err != nil {
				return err
			}
		}
		if frame.Epoch != replicaEpoch || frame.From != replicaOffset || frame.SnapshotID != replicaV2.snapshotID || frame.SnapshotOffset != replicaV2.snapshotReceived || frame.SnapshotBytes != replicaV2.snapshotBytes || frame.To != replicaV2.snapshotBase {
			return errors.New("snapshot chunk gap or identity mismatch")
		}
	} else {
		if !replicaV2.trusted || frame.Epoch != replicaEpoch || frame.From != replicaOffset || frame.To < frame.From || frame.To-frame.From != uint64(len(frame.Body)) || frame.SnapshotID != "" || frame.SnapshotDone {
			return errors.New("replication operation offset gap")
		}
	}
	if len(replicaV2.pending)+len(frame.Body) > replicationCommandBytes {
		return errors.New("replication command exceeds 64 MiB")
	}
	replicaV2.pending = append(replicaV2.pending, frame.Body...)
	var commands []*Command
	used := 0
	for used < len(replicaV2.pending) {
		cmd, n, parseErr := ParseCmd(replicaV2.pending[used:])
		if errors.Is(parseErr, ErrIncompleteFrame) {
			break
		}
		if parseErr != nil || n <= 0 {
			return errors.New("malformed replication operation")
		}
		switch cmd.Cmd {
		case "FLUSHDB", "DEL", "SET", "MSET", "INCR", "INCRBY", "DECR", "DECRBY", "PEXPIREAT", "PERSIST",
			"HSET", "HSETNX", "HDEL", "HINCRBY", "LPUSH", "RPUSH", "LPOP", "RPOP", "LTRIM", "LSET",
			"SADD", "SREM", "ZADD", "ZREM", "GEOADD", "KEEL.RESTORE":
		default:
			return fmt.Errorf("invalid replication operation %s", cmd.Cmd)
		}
		commands = append(commands, cmd)
		used += n
	}
	if (frame.Full && frame.SnapshotDone || !frame.Full && frame.CaughtUp) && used != len(replicaV2.pending) {
		return errors.New("replication prefix ends in an incomplete command")
	}
	replicaApplying = true
	defer func() { replicaApplying = false }()
	for _, cmd := range commands {
		var reply replicationReply
		if err := EvalAndResponse(cmd, &reply); err != nil {
			return err
		}
		if len(reply) > 0 && reply[0] == '-' {
			return fmt.Errorf("replication apply: %s", reply)
		}
	}
	if used == len(replicaV2.pending) {
		replicaV2.pending = nil
	} else if used > 0 {
		replicaV2.pending = append([]byte(nil), replicaV2.pending[used:]...)
	}
	if frame.Full {
		replicaV2.snapshotReceived += uint64(len(frame.Body))
		if frame.SnapshotDone {
			replicaV2.trusted = true
			replicaV2.snapshotID = ""
			replicaOffset = frame.To
		}
		// A snapshot completion alone is not a catch-up confirmation.
		return nil
	}
	replicaOffset = frame.To
	if frame.CaughtUp {
		if config.ReplicaOf != "" {
			if err := saveReplicaCheckpoint(); err != nil {
				return err
			}
		}
		replicaReady = true
	}
	replicaUpdated = time.Now()
	return nil
}
