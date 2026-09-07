package core

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

import "github.com/brandopakel/keel/internal/config"

// Protocol 2 uses a bounded byte history of canonical operations and a frozen
// rewrite descriptor for snapshots. File offsets identify snapshot chunks only;
// replication positions belong to the primary epoch and survive rewrites.
const replicationChunkBytes = 256 << 10
const replicationCommandBytes = 64 << 20

type replicationPiece struct {
	from uint64
	body []byte
}

var replicationV2 struct {
	end               uint64
	history           []replicationPiece
	bytes             int
	snapshot          *os.File
	snapshotID        string
	snapshotBytes     int64
	snapshotBase      uint64
	snapshotRequested bool
	failed            error
}

func closeReplicationSnapshot() {
	if replicationV2.snapshot != nil {
		replicationV2.snapshot.Close()
		replicationV2.snapshot = nil
	}
	replicationV2.snapshotID = ""
	replicationV2.snapshotBytes = 0
	replicationV2.snapshotBase = 0
}
func resetReplicationV2() {
	closeReplicationSnapshot()
	replicationV2.end = 0
	replicationV2.history = nil
	replicationV2.bytes = 0
	replicationV2.snapshotRequested = false
	replicationV2.failed = nil
	resetReplicaV2()
}
func replicationV2Enabled() bool {
	return config.ReplicationFeed && config.ReplicationProtocol == 2 && !aof.replaying && !replicaApplying
}

func invalidateReplicationV2() {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		replicationV2.failed = err
		return
	}
	replication.epoch = hex.EncodeToString(id[:])
	closeReplicationSnapshot()
	replicationV2.history = nil
	replicationV2.bytes = 0
	replicationV2.end = 0
	replication.invalidated = false
}

func recordReplicationV2Body(body []byte) {
	if !replicationV2Enabled() {
		return
	}
	for len(body) > 0 {
		// Pack small commands into byte-sized pieces. A per-command entry cap
		// would otherwise discard history after only milliseconds of busy traffic.
		last := len(replicationV2.history) - 1
		if last < 0 || len(replicationV2.history[last].body) == replicationChunkBytes {
			replicationV2.history = append(replicationV2.history, replicationPiece{from: replicationV2.end})
			last++
		}
		piece := &replicationV2.history[last]
		n := min(len(body), replicationChunkBytes-len(piece.body))
		piece.body = append(piece.body, body[:n]...)
		replicationV2.end += uint64(n)
		replicationV2.bytes += n
		body = body[n:]
		for replicationV2.bytes > replicationHistoryLimit || len(replicationV2.history) > 4096 {
			replicationV2.bytes -= len(replicationV2.history[0].body)
			replicationV2.history[0] = replicationPiece{}
			replicationV2.history = replicationV2.history[1:]
		}
	}
}

func recordReplicationV2Commit(cmd *Command) {
	if !replicationV2Enabled() {
		return
	}
	defer func() { clear(replication.dirty); replication.dirtyBytes = 0 }()
	if replication.invalidated {
		invalidateReplicationV2()
		return
	}
	if aof.commandStart >= len(aof.buf) {
		return
	}
	body := aof.buf[aof.commandStart:]
	// Filters/sketches may choose random seeds internally. Preserve exact state
	// for these commands; common strings/collections use their canonical deltas.
	opaque := strings.HasPrefix(cmd.Cmd, "BF.") || strings.HasPrefix(cmd.Cmd, "CF.") ||
		strings.HasPrefix(cmd.Cmd, "CMS.") || strings.HasPrefix(cmd.Cmd, "MORRIS.") ||
		cmd.Cmd == "PFADD" || cmd.Cmd == "PFMERGE"
	if opaque {
		var fits bool
		body, fits = opaqueReplicationBody()
		if !fits {
			invalidateReplicationV2()
			return
		}
	}
	recordReplicationV2Body(body)
}

// captureReplicationSnapshot runs after a rewrite's rename and directory sync,
// with all append batches fenced. Appends to the same inode cannot change its
// captured prefix; a later rewrite leaves this read-only descriptor valid.
func captureReplicationSnapshot() error {
	if !replicationV2Enabled() || !replicationV2.snapshotRequested {
		return nil
	}
	f, err := os.Open(aof.path)
	if err != nil {
		return err
	}
	var id [16]byte
	if _, err = rand.Read(id[:]); err != nil {
		f.Close()
		return err
	}
	closeReplicationSnapshot()
	replicationV2.snapshot = f
	replicationV2.snapshotID = hex.EncodeToString(id[:])
	replicationV2.snapshotBytes = aof.baseSize
	replicationV2.snapshotBase = replicationV2.end
	replicationV2.snapshotRequested = false
	return nil
}

func historyV2Contains(offset uint64) bool {
	if offset > replicationV2.end {
		return false
	}
	if len(replicationV2.history) == 0 {
		return offset == replicationV2.end
	}
	return offset >= replicationV2.history[0].from
}

func encodeReplicationFrame(frame ReplicationFrame) []byte {
	frame.Checksum = frameChecksum(frame)
	body, err := json.Marshal(frame)
	if err != nil {
		return Encode(err, false)
	}
	return Encode(string(body), false)
}

// KEEL.REPL.PULL2 epoch byte-offset snapshot-id snapshot-byte-offset.
// An explicit command and version prevent older peers interpreting new frames.
func cmdReplicationPullV2(args []string) []byte {
	if !config.ReplicationFeed || config.ReplicationProtocol != 2 {
		return Encode(errors.New("ERR replication protocol 2 is disabled"), false)
	}
	if len(args) != 5 {
		return Encode(errSyntax, false)
	}
	// The caller's term arrives on every pull, so a primary that has been
	// replaced finds out from the first replica that has moved on, without
	// waiting for a coordinator to remember to tell it.
	callerTerm, termErr := strconv.ParseUint(args[4], 10, 64)
	if termErr != nil {
		return Encode(errNotAnInteger, false)
	}
	if err := observeTerm(callerTerm); err != nil {
		return Encode(fmt.Errorf("ERR recording term: %w", err), false)
	}
	if !Writable() {
		// A deposed primary must stop feeding replicas as well as stop taking
		// writes: serving its own history would hand a replica a past the
		// cluster has left.
		return Encode(errFenced, false)
	}
	offset, e1 := strconv.ParseUint(args[1], 10, 64)
	part, e2 := strconv.ParseUint(args[3], 10, 64)
	if e1 != nil || e2 != nil {
		return Encode(errNotAnInteger, false)
	}
	if replicationV2.failed != nil {
		return Encode(replicationV2.failed, false)
	}
	frame := ReplicationFrame{Version: 2, Epoch: replication.epoch, From: offset, To: offset, Term: failover.term}
	full := args[2] != "" || args[0] != replication.epoch || !historyV2Contains(offset)
	if !full {
		if part != 0 {
			return Encode(errSyntax, false)
		}
		for _, piece := range replicationV2.history {
			end := piece.from + uint64(len(piece.body))
			if end <= frame.To {
				continue
			}
			if piece.from > frame.To {
				return Encode(errors.New("ERR replication history gap"), false)
			}
			begin := int(frame.To - piece.from)
			n := min(len(piece.body)-begin, replicationChunkBytes-len(frame.Body))
			frame.Body = append(frame.Body, piece.body[begin:begin+n]...)
			frame.To += uint64(n)
			if len(frame.Body) == replicationChunkBytes {
				break
			}
		}
		frame.CaughtUp = frame.To == replicationV2.end
		return encodeReplicationFrame(frame)
	}
	frame.Full = true
	valid := replicationV2.snapshot != nil && historyV2Contains(replicationV2.snapshotBase)
	if args[2] != "" && (!valid || args[2] != replicationV2.snapshotID) {
		frame.Pending = true
		return encodeReplicationFrame(frame) // client discards its stale transfer cursor
	}
	if !valid {
		closeReplicationSnapshot()
		replicationV2.snapshotRequested = true
		if !RewriteActive() {
			if err := StartRewrite(); err != nil {
				return Encode(fmt.Errorf("ERR preparing replication snapshot: %w", err), false)
			}
		}
		frame.Pending = true
		return encodeReplicationFrame(frame)
	}
	if replicationV2.snapshotBytes > maxReplicationSnapshotBytes {
		return Encode(errors.New("ERR replication snapshot exceeds 1 GiB"), false)
	}
	if args[2] == "" && part != 0 {
		return Encode(errSyntax, false)
	}
	if part > uint64(replicationV2.snapshotBytes) {
		return Encode(errors.New("ERR invalid snapshot position"), false)
	}
	n := min(int64(replicationChunkBytes), replicationV2.snapshotBytes-int64(part))
	frame.Body = make([]byte, n)
	if n > 0 {
		got, err := replicationV2.snapshot.ReadAt(frame.Body, int64(part))
		if err != nil && err != io.EOF {
			return Encode(err, false)
		}
		if got != int(n) {
			return Encode(io.ErrUnexpectedEOF, false)
		}
	}
	frame.To = replicationV2.snapshotBase
	frame.SnapshotID = replicationV2.snapshotID
	frame.SnapshotOffset = part
	frame.SnapshotBytes = uint64(replicationV2.snapshotBytes)
	frame.SnapshotDone = part+uint64(n) == frame.SnapshotBytes
	return encodeReplicationFrame(frame)
}
