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
	Version  int    `json:"version"`
	Epoch    string `json:"epoch"`
	From     uint64 `json:"from"`
	To       uint64 `json:"to"`
	Full     bool   `json:"full"`
	Body     []byte `json:"body"`
	Checksum string `json:"checksum"`
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
var replicaApplying bool
var replicaReady bool
var replicaEpoch string
var replicaOffset uint64
var replicaUpdated time.Time

func InitReplication() error {
	replication.dirty = make(map[string]struct{})
	replication.history = nil
	replication.bytes = 0
	replication.offset = 0
	replication.dirtyBytes = 0
	replication.invalidated = false
	replicaReady = false
	replicaEpoch = ""
	replicaOffset = 0
	replicaUpdated = time.Time{}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	replication.epoch = hex.EncodeToString(id[:])
	if config.ReplicaOf != "" {
		data_structure.SuspendExpiry = true
		data_structure.SuspendEviction = true
	}
	return nil
}
func noteReplicationDirty(key string) {
	if config.ReplicationFeed && !aof.replaying && !replicaApplying {
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
		body = emitKey(body, key)
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
	encoded, _ := json.Marshal(f)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}
func cmdReplicationPull(args []string) []byte {
	if !config.ReplicationFeed {
		return Encode(errors.New("ERR replication feed is disabled"), false)
	}
	if len(args) != 2 {
		return Encode(errSyntax, false)
	}
	offset, err := strconv.ParseUint(args[1], 10, 64)
	if err != nil {
		return Encode(errNotAnInteger, false)
	}
	if err := sealReplication(); err != nil {
		return Encode(err, false)
	}
	full := args[0] != replication.epoch || offset > replication.offset
	if len(replication.history) > 0 && offset < replication.history[0].offset-1 {
		full = true
	}
	frame := ReplicationFrame{Version: 1, Epoch: replication.epoch, From: offset, To: offset, Full: full}
	if full {
		if data_structure.TotalMemUsed() > replicationLimit || data_structure.TotalKeys() > 100000 {
			return Encode(errors.New("ERR replication snapshot limit"), false)
		}
		frame.Body = appendCommand(nil, "FLUSHDB")
		for _, key := range allKeyNames() {
			frame.Body = emitKey(frame.Body, key)
			if len(frame.Body) > replicationLimit {
				return Encode(errors.New("ERR replication snapshot exceeds 8 MiB"), false)
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
		return Encode(err, false)
	}
	return Encode(string(encoded), false)
}

// ApplyReplication is called only on the event loop. Any failure is fatal to
// the replica: it must not serve a partially applied frame. Restart requires a
// fresh full sync before reads are enabled, regardless of local AOF contents.
func ApplyReplication(frame ReplicationFrame) error {
	if frame.Version != 1 || len(frame.Epoch) != 32 || len(frame.Body) > replicationLimit || frame.Checksum != frameChecksum(frame) {
		return errors.New("invalid replication frame")
	}
	if _, err := hex.DecodeString(frame.Epoch); err != nil {
		return err
	}
	if !frame.Full && (!replicaReady || frame.Epoch != replicaEpoch || frame.From != replicaOffset || frame.To < frame.From) {
		return errors.New("replication offset gap")
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
	replicaReady = true
	replicaEpoch = frame.Epoch
	replicaOffset = frame.To
	replicaUpdated = time.Now()
	return nil
}

type replicationReply []byte

func (r *replicationReply) Write(b []byte) (int, error) { *r = append(*r, b...); return len(b), nil }
func (r *replicationReply) Read(b []byte) (int, error)  { return 0, errors.New("read unsupported") }

func replicaCommandError(cmd string) error {
	if config.ReplicaOf == "" || replicaApplying || aof.replaying {
		return nil
	}
	if writeCommands[cmd] {
		return errors.New("READONLY replica rejects writes")
	}
	if cmd != "PING" && cmd != "INFO" && (!replicaReady || time.Since(replicaUpdated) > 5*time.Second) {
		return errors.New("MASTERDOWN replica has no recent primary state")
	}
	return nil
}
