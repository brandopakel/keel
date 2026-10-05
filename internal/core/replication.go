package core

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/brandopakel/keel/internal/config"
	"github.com/brandopakel/keel/internal/data_structure"
)

// Experimental bounded replication uses canonical key images, not random
// commands. Offsets belong to an epoch and survive AOF rewrites, not restarts.
const replicationLimit = 8 << 20
const replicationHistoryLimit = 16 << 20

type ReplicationFrame struct {
	Version        int    `json:"version"`
	Epoch          string `json:"epoch"`
	From           uint64 `json:"from"`
	To             uint64 `json:"to"`
	Full           bool   `json:"full"`
	Body           []byte `json:"body"`
	Checksum       string `json:"checksum"`
	SnapshotID     string `json:"snapshot_id,omitempty"`
	SnapshotOffset uint64 `json:"snapshot_offset,omitempty"`
	SnapshotBytes  uint64 `json:"snapshot_bytes,omitempty"`
	SnapshotDone   bool   `json:"snapshot_done,omitempty"`
	Pending        bool   `json:"pending,omitempty"`
	CaughtUp       bool   `json:"caught_up,omitempty"`
	// Term is the authority the sender believes it has. frameChecksum marshals
	// the whole struct, so this is covered by the integrity checksum. Peer
	// authentication and transport protection are separate requirements.
	Term uint64 `json:"term,omitempty"`
}
type replicationBatch struct {
	offset uint64
	body   []byte
}

var replication struct {
	epoch       string
	offset      uint64
	dirty       map[string]struct{}
	history     []replicationBatch
	bytes       int
	dirtyBytes  int
	invalidated bool
}

// InitReplication starts replication on the default engine once its log is
// open, as the server does: a primary's stream in a new epoch, and a replica
// that has applied nothing yet and, if it follows a primary, holds off expiry
// and eviction and reads the checkpoint it restarts from. The primary's
// stream is still package state until plan step 2.4 moves it too.
func InitReplication() error {
	resetReplicationV2()
	replication.dirty = make(map[string]struct{})
	replication.history = nil
	replication.bytes = 0
	replication.offset = 0
	replication.dirtyBytes = 0
	replication.invalidated = false
	defaultEngine.resetReplica()
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	replication.epoch = hex.EncodeToString(id[:])
	return defaultEngine.followPrimary()
}

// resetReplica forgets everything e has applied from a primary: it is not
// ready, at no epoch or offset, and holds no partial snapshot, operation or
// transaction.
func (e *Engine) resetReplica() {
	e.resetReplicaV2()
	e.replicaReady = false
	e.replicaEpoch = ""
	e.replicaOffset = 0
	e.replicaUpdated = time.Time{}
}

// followPrimary readies e to apply a primary's stream, if it is a replica: its
// space leaves expiry and eviction to the primary, and under protocol 2 the
// checkpoint beside its log says where it may resume.
func (e *Engine) followPrimary() error {
	if config.ReplicaOf != "" {
		e.space.SuspendExpiry = true
		e.space.SuspendEviction = true
	}
	if config.ReplicaOf != "" && config.ReplicationProtocol == 2 {
		return e.loadReplicaCheckpoint()
	}
	return nil
}
func (e *Engine) noteReplicationDirty(key string) {
	if config.ReplicationFeed && !e.aof.replaying && !e.replicaApplying {
		if replication.invalidated {
			return
		}
		if _, exists := replication.dirty[key]; exists {
			return
		}
		if len(replication.dirty) >= 100000 || replication.dirtyBytes+len(key) > replicationLimit {
			clear(replication.dirty)
			replication.dirtyBytes = 0
			replication.invalidated = true
			return
		}
		if replication.dirty == nil {
			replication.dirty = make(map[string]struct{})
		}
		replication.dirty[key] = struct{}{}
		replication.dirtyBytes += len(key)
	}
}
func sealReplication() error {
	if replication.invalidated {
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return err
		}
		replication.epoch = hex.EncodeToString(id[:])
		replication.offset = 0
		replication.history = nil
		replication.bytes = 0
		replication.invalidated = false
	}
	if len(replication.dirty) == 0 {
		return nil
	}
	if data_structure.TotalMemUsed() > replicationLimit || len(replication.dirty) > 100000 {
		return errors.New("replication alpha dataset limit: 8 MiB estimated keyspace / 100000 changed keys")
	}
	var body []byte
	for key := range replication.dirty {
		body = appendCommand(body, "DEL", key)
		body = defaultEngine.emitKey(body, key)
		if len(body) > replicationLimit {
			return errors.New("replication frame exceeds 8 MiB")
		}
	}
	replication.offset++
	replication.history = append(replication.history, replicationBatch{replication.offset, body})
	replication.bytes += len(body)
	clear(replication.dirty)
	replication.dirtyBytes = 0
	for replication.bytes > replicationHistoryLimit || len(replication.history) > 1024 {
		replication.bytes -= len(replication.history[0].body)
		replication.history[0] = replicationBatch{}
		replication.history = replication.history[1:]
	}
	return nil
}
func frameChecksum(f ReplicationFrame) string {
	// Include ordering metadata, not only the payload, in the integrity check.
	f.Checksum = ""
	// All fields are JSON-supported scalars or []byte; there are no custom
	// marshalers, cycles, floats or interface values that could make this fail.
	encoded, _ := json.Marshal(f)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}
func (e *Engine) cmdReplicationPull(args []string) []byte {
	if !config.ReplicationFeed || config.ReplicationProtocol != 1 {
		return e.encode(errors.New("ERR replication protocol 1 is disabled"), false)
	}
	if CurrentTerm() != 0 || !Writable() {
		return e.encode(errors.New("ERR nonzero terms require replication protocol 2"), false)
	}
	if len(args) != 2 {
		return e.encode(wrongArguments("KEEL.REPL.PULL"), false)
	}
	offset, err := strconv.ParseUint(args[1], 10, 64)
	if err != nil {
		return e.encode(errNotAnInteger, false)
	}
	if err := sealReplication(); err != nil {
		return e.encode(err, false)
	}
	full := args[0] != replication.epoch || offset > replication.offset
	if len(replication.history) > 0 && offset < replication.history[0].offset-1 {
		full = true
	}
	frame := ReplicationFrame{Version: 1, Epoch: replication.epoch, From: offset, To: offset, Full: full}
	if full {
		if data_structure.TotalMemUsed() > replicationLimit || data_structure.TotalKeys() > 100000 {
			return e.encode(errors.New("ERR replication snapshot limit"), false)
		}
		frame.Body = appendCommand(nil, "FLUSHDB")
		walk := data_structure.NewKeyspaceWalk()
		var keys []string
		for !walk.Done() {
			var err error
			keys, _, err = walk.Next(1024, keys[:0])
			if err != nil {
				return e.encode(err, false)
			}
			for _, key := range keys {
				frame.Body = e.emitKey(frame.Body, key)
				if len(frame.Body) > replicationLimit {
					return e.encode(errors.New("ERR replication snapshot exceeds 8 MiB"), false)
				}
			}
		}
		frame.To = replication.offset
	} else {
		for _, batch := range replication.history {
			if batch.offset <= offset {
				continue
			}
			if len(frame.Body)+len(batch.body) > replicationLimit {
				break
			}
			frame.Body = append(frame.Body, batch.body...)
			frame.To = batch.offset
		}
	}
	frame.Checksum = frameChecksum(frame)
	encoded, err := json.Marshal(frame)
	if err != nil {
		return e.encode(err, false)
	}
	return e.encode(string(encoded), false)
}

// ApplyReplication is called only on the event loop. Any failure is fatal to
// the replica: it must not serve a partially applied frame. Restart requires a
// fresh full sync before reads are enabled, regardless of local AOF contents.
func ApplyReplication(frame ReplicationFrame) error { return defaultEngine.ApplyReplication(frame) }

// ApplyReplication is the package's ApplyReplication on e: the frame is
// applied to e's keyspace, logged in e's log, and moves e's position in its
// primary's stream.
func (e *Engine) ApplyReplication(frame ReplicationFrame) error {
	if config.ReplicationProtocol == 2 {
		return e.applyReplicationV2(frame)
	}
	if CurrentTerm() != 0 || frame.Term != 0 {
		e.replicaReady = false
		return errors.New("nonzero terms require replication protocol 2")
	}
	if frame.Version != 1 || len(frame.Epoch) != 32 || len(frame.Body) > replicationLimit || frame.Checksum != frameChecksum(frame) {
		return errors.New("invalid replication frame")
	}
	if _, err := hex.DecodeString(frame.Epoch); err != nil {
		return err
	}
	if !frame.Full && (!e.replicaReady || frame.Epoch != e.replicaEpoch || frame.From != e.replicaOffset || frame.To < frame.From) {
		return errors.New("replication offset gap")
	}
	if frame.Full && e.replicaReady && frame.From != e.replicaOffset {
		return errors.New("snapshot response does not match requested offset")
	}
	if !frame.Full && ((len(frame.Body) == 0 && frame.To != frame.From) || (len(frame.Body) > 0 && frame.To == frame.From)) {
		return errors.New("replication payload and offset disagree")
	}
	var commands []*Command
	body := frame.Body
	for len(body) > 0 {
		cmd, n, err := ParseCmd(body)
		if err != nil || n <= 0 {
			return errors.New("malformed replication command")
		}
		switch cmd.Cmd {
		case "FLUSHDB", "DEL", "SET", "SADD", "HSET", "RPUSH", "ZADD", "KEEL.RESTORE", "PEXPIREAT":
		default:
			return fmt.Errorf("invalid replication command %s", cmd.Cmd)
		}
		if cmd.Cmd == "FLUSHDB" && (!frame.Full || len(commands) != 0) {
			return errors.New("unexpected replication flush")
		}
		commands = append(commands, cmd)
		body = body[n:]
	}
	if frame.Full && (len(commands) == 0 || commands[0].Cmd != "FLUSHDB") {
		return errors.New("snapshot lacks reset")
	}
	// The server stops on an apply error. Also gate reads here so partial
	// state cannot be read or resumed with a delta by another caller.
	e.replicaReady = false
	e.replicaApplying = true
	defer func() { e.replicaApplying = false }()
	for _, cmd := range commands {
		var reply replicationReply
		if err := e.evalAndResponse(cmd, &reply); err != nil {
			return err
		}
		if len(reply) > 0 && reply[0] == '-' {
			return fmt.Errorf("replication apply: %s", reply)
		}
	}
	e.replicaReady = true
	e.replicaEpoch = frame.Epoch
	e.replicaOffset = frame.To
	e.replicaUpdated = time.Now()
	return nil
}

type replicationReply []byte

func (r *replicationReply) Write(b []byte) (int, error) { *r = append(*r, b...); return len(b), nil }
func (r *replicationReply) Read(b []byte) (int, error)  { return 0, errors.New("read unsupported") }

// answersWithoutData are the commands a replica can answer without primary
// state, because what they reply does not come from the dataset. ECHO and
// SELECT are here because clients send them while setting up a connection, and
// a replica that refused them would fail the connection rather than the read;
// UNWATCH because they send it while putting one back.
var answersWithoutData = map[string]bool{"PING": true, "INFO": true, "ECHO": true, "SELECT": true, "UNWATCH": true}

var errReadOnlyReplica = errors.New("READONLY You can't write against a read only replica.")

func (e *Engine) replicaCommandError(cmd string) error {
	// Applying replicated state and replaying the log are not client writes:
	// one is a decision the primary already made, the other is recovery.
	if e.replicaApplying || e.aof.replaying {
		return nil
	}
	if config.ReplicaOf != "" {
		if writeCommands[cmd] {
			// READONLY rather than FENCED even when this replica has seen a
			// term above its own: a replica refuses writes because of what it
			// is, and saying so is more use to a client than saying it lost an
			// election it was never in. Worded as a Redis replica words it.
			return errReadOnlyReplica
		}
		if !answersWithoutData[cmd] && (!e.replicaReady || time.Since(e.replicaUpdated) > 5*time.Second) {
			return errors.New("MASTERDOWN replica has no recent primary state")
		}
		return nil
	}
	// Checked on every write rather than at a transition, so there is no window
	// between losing authority and noticing it. Writable first: it is a few
	// fields, and a writable primary then never looks the command up at all.
	if !Writable() && writeCommands[cmd] {
		return errFenced
	}
	return nil
}
