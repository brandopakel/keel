package core

import (
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/brandopakel/keel/internal/config"
)

const maxReplicationSnapshotBytes = 1 << 30

// replicaV2State is what a protocol 2 replica holds of its primary's stream
// between frames: whether its keyspace is a trusted prefix of the stream, the
// snapshot it is receiving, an operation or a transaction not yet whole, and
// the checkpoint it last wrote.
type replicaV2State struct {
	trusted                                       bool
	resumed                                       bool
	snapshotID                                    string
	snapshotBytes, snapshotReceived, snapshotBase uint64
	pending                                       []byte
	// block is a transaction whose EXEC has not arrived yet, parsed as far as
	// the stream has reached and held back from the keyspace until it has;
	// blockBytes is how much of the stream it has taken.
	block      []*Command
	blockOpen  bool
	blockBytes int
	checkpoint replicaCheckpoint
}

func (e *Engine) resetReplicaV2() {
	e.replicaV2.trusted = false
	e.replicaV2.resumed = false
	e.replicaV2.snapshotID = ""
	e.replicaV2.snapshotBytes = 0
	e.replicaV2.snapshotReceived = 0
	e.replicaV2.snapshotBase = 0
	e.replicaV2.pending = nil
	e.replicaV2.block, e.replicaV2.blockOpen, e.replicaV2.blockBytes = nil, false, 0
	e.replicaV2.checkpoint = replicaCheckpoint{}
}

func (e *Engine) applyReplicationV2(frame ReplicationFrame) (err error) {
	if e.AppendPending() {
		return errors.New("replication apply waits for pending append")
	}
	defer func() {
		if err != nil {
			e.replicaReady = false
			e.replicaV2.trusted = false
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
		e.replicaReady = false
		e.resetReplicaV2()
		e.replicaEpoch = ""
		e.replicaOffset = 0
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
			if frame.From != e.replicaOffset {
				return errors.New("snapshot does not match requested offset")
			}
			e.replicaReady = false
			e.resetReplicaV2()
			e.replicaV2.snapshotID = frame.SnapshotID
			e.replicaV2.snapshotBytes = frame.SnapshotBytes
			e.replicaV2.snapshotBase = frame.To
			e.replicaEpoch = frame.Epoch
			// The snapshot is an AOF prefix, not an independently readable frame.
			// Gate the old state before reset and keep reads gated until catch-up.
			e.replicaApplying = true
			var reply replicationReply
			err := e.evalAndResponse(&Command{Cmd: "FLUSHDB"}, &reply)
			e.replicaApplying = false
			if err != nil {
				return err
			}
		}
		if frame.Epoch != e.replicaEpoch || frame.From != e.replicaOffset || frame.SnapshotID != e.replicaV2.snapshotID || frame.SnapshotOffset != e.replicaV2.snapshotReceived || frame.SnapshotBytes != e.replicaV2.snapshotBytes || frame.To != e.replicaV2.snapshotBase {
			return errors.New("snapshot chunk gap or identity mismatch")
		}
	} else {
		if !e.replicaV2.trusted || frame.Epoch != e.replicaEpoch || frame.From != e.replicaOffset || frame.To < frame.From || frame.To-frame.From != uint64(len(frame.Body)) || frame.SnapshotID != "" || frame.SnapshotDone {
			return errors.New("replication operation offset gap")
		}
	}
	// An open transaction counts against the same bound as an incomplete
	// command: both are stream the replica holds and has not applied.
	if e.replicaV2.blockBytes+len(e.replicaV2.pending)+len(frame.Body) > replicationCommandBytes {
		return errors.New("replication command exceeds 64 MiB")
	}
	e.replicaV2.pending = append(e.replicaV2.pending, frame.Body...)
	// Each unit is applied as a whole: an ordinary operation on its own, or a
	// transaction's block once its EXEC is here. A block's commands are moved
	// out of pending as they are parsed, so a large one arriving over many
	// frames is parsed once rather than again with every frame.
	var units []replicationUnit
	used := 0
	for used < len(e.replicaV2.pending) {
		cmd, n, parseErr := ParseCmd(e.replicaV2.pending[used:])
		if errors.Is(parseErr, ErrIncompleteFrame) {
			break
		}
		if parseErr != nil || n <= 0 {
			return errors.New("malformed replication operation")
		}
		used += n
		switch cmd.Cmd {
		case "MULTI":
			if e.replicaV2.blockOpen || len(cmd.Args) != 0 {
				return errors.New("malformed replication transaction")
			}
			e.replicaV2.blockOpen, e.replicaV2.blockBytes = true, n
			continue
		case "EXEC":
			if !e.replicaV2.blockOpen || len(cmd.Args) != 0 {
				return errors.New("malformed replication transaction")
			}
			units = append(units, replicationUnit{commands: e.replicaV2.block, transaction: true})
			e.replicaV2.block, e.replicaV2.blockOpen, e.replicaV2.blockBytes = nil, false, 0
			continue
		case "FLUSHDB", "DEL", "SET", "MSET", "INCR", "INCRBY", "DECR", "DECRBY", "PEXPIREAT", "PERSIST",
			"HSET", "HSETNX", "HDEL", "HINCRBY", "LPUSH", "RPUSH", "LPOP", "RPOP", "LTRIM", "LSET",
			"SADD", "SREM", "ZADD", "ZREM", "GEOADD", "KEEL.RESTORE":
		default:
			return fmt.Errorf("invalid replication operation %s", cmd.Cmd)
		}
		if e.replicaV2.blockOpen {
			e.replicaV2.block = append(e.replicaV2.block, cmd)
			e.replicaV2.blockBytes += n
		} else {
			units = append(units, replicationUnit{commands: []*Command{cmd}})
		}
	}
	if (frame.Full && frame.SnapshotDone || !frame.Full && frame.CaughtUp) && (used != len(e.replicaV2.pending) || e.replicaV2.blockOpen) {
		return errors.New("replication prefix ends in an incomplete command")
	}
	e.replicaApplying = true
	defer func() { e.replicaApplying = false }()
	for _, unit := range units {
		if err := unit.apply(e); err != nil {
			return err
		}
	}
	if used == len(e.replicaV2.pending) {
		e.replicaV2.pending = nil
	} else if used > 0 {
		e.replicaV2.pending = append([]byte(nil), e.replicaV2.pending[used:]...)
	}
	if frame.Full {
		e.replicaV2.snapshotReceived += uint64(len(frame.Body))
		if frame.SnapshotDone {
			e.replicaV2.trusted = true
			e.replicaV2.snapshotID = ""
			e.replicaOffset = frame.To
		}
		// A snapshot completion alone is not a catch-up confirmation.
		return nil
	}
	e.replicaOffset = frame.To
	if frame.CaughtUp {
		if config.ReplicaOf != "" {
			if err := e.saveReplicaCheckpoint(); err != nil {
				return err
			}
		}
		e.replicaReady = true
	}
	e.replicaUpdated = time.Now()
	return nil
}

type replicationUnit struct {
	commands    []*Command
	transaction bool
}

// apply runs a unit on e with the primary's decisions already made. A
// transaction runs as one, framed in e's own log as it was in the primary's, so
// a crash here cannot leave the replica's log holding half of it either.
func (u replicationUnit) apply(e *Engine) error {
	var failed error
	check := func(_ int, reply []byte, err error) {
		if failed != nil {
			return
		}
		if err != nil {
			failed = err
		} else if len(reply) > 0 && reply[0] == '-' {
			failed = fmt.Errorf("replication apply: %s", reply)
		}
	}
	if u.transaction {
		e.runTransaction(u.commands, e.evalAndResponse, check)
		return failed
	}
	var reply replicationReply
	err := e.evalAndResponse(u.commands[0], &reply)
	check(0, reply, err)
	return failed
}
