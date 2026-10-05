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

// Protocol 2 uses a bounded byte history of canonical operations and a frozen
// rewrite descriptor for snapshots. File offsets identify snapshot chunks only;
// replication positions belong to the primary epoch and survive rewrites.
const replicationChunkBytes = 256 << 10
const replicationCommandBytes = 64 << 20

type replicationPiece struct {
	from uint64
	body []byte
}

// replicationV2State is a primary's protocol 2 stream: the end of its
// history and the history a replica can still be served from, the snapshot
// a replica is fetching, and whether one has been asked for.
type replicationV2State struct {
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

func (e *Engine) closeReplicationSnapshot() {
	if e.replicationV2.snapshot != nil {
		e.replicationV2.snapshot.Close()
		e.replicationV2.snapshot = nil
	}
	e.replicationV2.snapshotID = ""
	e.replicationV2.snapshotBytes = 0
	e.replicationV2.snapshotBase = 0
}
func (e *Engine) resetReplicationV2() {
	e.resetReplicaAcknowledgement()
	e.closeReplicationSnapshot()
	e.replicationV2.end = 0
	e.replicationV2.history = nil
	e.replicationV2.bytes = 0
	e.replicationV2.snapshotRequested = false
	e.replicationV2.failed = nil
}
func (e *Engine) replicationV2Enabled() bool {
	return e.feedsReplicas() && e.replicationProtocol() == 2 && !e.aof.replaying && !e.replicaApplying
}

func (e *Engine) invalidateReplicationV2() {
	// A transaction running now loses its place in the stream with the rest
	// of the history; the snapshot that replaces it will contain all of it.
	if e.replicationTransaction.active {
		e.replicationTransaction.dropped = true
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		e.replicationV2.failed = err
		return
	}
	e.replication.epoch = hex.EncodeToString(id[:])
	e.resetReplicaAcknowledgement()
	e.closeReplicationSnapshot()
	e.replicationV2.history = nil
	e.replicationV2.bytes = 0
	e.replicationV2.end = 0
	e.replication.invalidated = false
}

func (e *Engine) recordReplicationV2Body(body []byte) {
	if !e.replicationV2Enabled() || len(body) == 0 {
		return
	}
	if e.replicationTransaction.active && !e.admitReplicationTransaction(len(body)) {
		return
	}
	e.appendReplicationV2History(body)
}

func (e *Engine) appendReplicationV2History(body []byte) {
	for len(body) > 0 {
		// Pack small commands into byte-sized pieces. A per-command entry cap
		// would otherwise discard history after only milliseconds of busy traffic.
		last := len(e.replicationV2.history) - 1
		if last < 0 || len(e.replicationV2.history[last].body) == replicationChunkBytes {
			e.replicationV2.history = append(e.replicationV2.history, replicationPiece{from: e.replicationV2.end})
			last++
		}
		piece := &e.replicationV2.history[last]
		n := min(len(body), replicationChunkBytes-len(piece.body))
		piece.body = append(piece.body, body[:n]...)
		e.replicationV2.end += uint64(n)
		e.replicationV2.bytes += n
		body = body[n:]
		for e.replicationV2.bytes > replicationHistoryLimit || len(e.replicationV2.history) > 4096 {
			e.replicationV2.bytes -= len(e.replicationV2.history[0].body)
			e.replicationV2.history[0] = replicationPiece{}
			e.replicationV2.history = e.replicationV2.history[1:]
		}
	}
}

func (e *Engine) recordReplicationV2Commit() {
	if !e.replicationV2Enabled() {
		return
	}
	e.publishReplicationV2Commit()
	// Published, or dropped with the epoch they belonged to: either way the
	// keys the command changed are done with. Cleared here rather than in a
	// deferred closure, which would be called through on every write; no
	// panic is recovered on the way out of a command, so the two do not
	// differ.
	clear(e.replication.dirty)
	e.replication.dirtyBytes = 0
}

// publishReplicationV2Commit publishes what the command that just ran
// changed: its log records, or the exact images of the keys it changed, or,
// when those cannot fit, a new epoch.
func (e *Engine) publishReplicationV2Commit() {
	if e.replication.invalidated {
		e.invalidateReplicationV2()
		return
	}
	if !e.aof.commandChanged {
		return
	}
	body := e.aof.buf[e.aof.commandStart:]
	// Filters/sketches may choose random seeds internally. Preserve exact state
	// for these commands; common strings/collections use their canonical deltas.
	if e.aof.commandOpaque {
		var fits bool
		body, fits = e.opaqueReplicationBody()
		if !fits {
			e.invalidateReplicationV2()
			return
		}
	}
	e.recordReplicationV2Body(body)
}

func isOpaqueReplicationCommand(name string) bool {
	return strings.HasPrefix(name, "BF.") || strings.HasPrefix(name, "CF.") ||
		strings.HasPrefix(name, "CMS.") || strings.HasPrefix(name, "MORRIS.") ||
		name == "PFADD" || name == "PFMERGE"
}

// captureReplicationSnapshot runs after a rewrite's rename and directory sync,
// with all append batches fenced. Appends to the same inode cannot change its
// captured prefix; a later rewrite leaves this read-only descriptor valid.
func (e *Engine) captureReplicationSnapshot() error {
	if !e.replicationV2Enabled() || !e.replicationV2.snapshotRequested {
		return nil
	}
	f, err := os.Open(e.aof.path)
	if err != nil {
		return err
	}
	var id [16]byte
	if _, err = rand.Read(id[:]); err != nil {
		f.Close()
		return err
	}
	e.closeReplicationSnapshot()
	e.replicationV2.snapshot = f
	e.replicationV2.snapshotID = hex.EncodeToString(id[:])
	e.replicationV2.snapshotBytes = e.aof.baseSize
	e.replicationV2.snapshotBase = e.replicationV2.end
	e.replicationV2.snapshotRequested = false
	return nil
}

func (e *Engine) historyV2Contains(offset uint64) bool {
	if offset > e.replicationV2.end {
		return false
	}
	if len(e.replicationV2.history) == 0 {
		return offset == e.replicationV2.end
	}
	return offset >= e.replicationV2.history[0].from
}

func encodeReplicationFrame(frame ReplicationFrame) []byte {
	frame.Checksum = frameChecksum(frame)
	body, err := json.Marshal(frame)
	if err != nil {
		return Encode(err, false)
	}
	return Encode(string(body), false)
}

// ReplicationTermRequiredReply is a capability signal, not a term grant. A new
// peer retries with an explicit fifth argument; old peers fail before frame decode.
const ReplicationTermRequiredReply = "-REPLTERM term-aware protocol 2 request required\r\n"

// KEEL.REPL.PULL2 epoch byte-offset snapshot-id snapshot-byte-offset [term].
// An explicit command and version prevent older peers interpreting new frames.
func (e *Engine) cmdReplicationPullV2(args []string) []byte {
	if !e.feedsReplicas() || e.replicationProtocol() != 2 {
		return e.encode(errors.New("ERR replication protocol 2 is disabled"), false)
	}
	if len(args) != 4 && len(args) != 5 {
		return e.encode(wrongArguments("KEEL.REPL.PULL2"), false)
	}
	if len(args) == 4 && e.CurrentTerm() != 0 {
		return []byte(ReplicationTermRequiredReply)
	}
	// The caller's term arrives on every pull, so a primary that has been
	// replaced finds out from the first replica that has moved on, without
	// waiting for a coordinator to remember to tell it.
	var callerTerm uint64
	if len(args) == 5 {
		var termErr error
		callerTerm, termErr = strconv.ParseUint(args[4], 10, 64)
		if termErr != nil {
			return e.encode(errNotAnInteger, false)
		}
	}
	if err := e.observeTerm(callerTerm); err != nil {
		return e.encode(fmt.Errorf("ERR recording term: %w", err), false)
	}
	if !e.writable() {
		// A deposed primary must stop feeding replicas as well as stop taking
		// writes: serving its own history would hand a replica a past the
		// cluster has left.
		return e.encode(errFenced, false)
	}
	offset, e1 := strconv.ParseUint(args[1], 10, 64)
	part, e2 := strconv.ParseUint(args[3], 10, 64)
	if e1 != nil || e2 != nil {
		return e.encode(errNotAnInteger, false)
	}
	if e.replicationV2.failed != nil {
		return e.encode(e.replicationV2.failed, false)
	}
	frame := ReplicationFrame{Version: 2, Epoch: e.replication.epoch, From: offset, To: offset, Term: e.failover.term}
	full := args[2] != "" || args[0] != e.replication.epoch || !e.historyV2Contains(offset)
	if !full {
		if part != 0 {
			return e.encode(errSyntax, false)
		}
		for _, piece := range e.replicationV2.history {
			end := piece.from + uint64(len(piece.body))
			if end <= frame.To {
				continue
			}
			if piece.from > frame.To {
				return e.encode(errors.New("ERR replication history gap"), false)
			}
			begin := int(frame.To - piece.from)
			n := min(len(piece.body)-begin, replicationChunkBytes-len(frame.Body))
			frame.Body = append(frame.Body, piece.body[begin:begin+n]...)
			frame.To += uint64(n)
			if len(frame.Body) == replicationChunkBytes {
				break
			}
		}
		frame.CaughtUp = frame.To == e.replicationV2.end
		// Only a successfully validated ordinary delta pull reports progress.
		// The received cursor can still end inside a buffered command; it is
		// not an applied/durable offset or a promotion-loss estimate.
		e.noteReplicaAcknowledged(offset)
		return encodeReplicationFrame(frame)
	}
	frame.Full = true
	valid := e.replicationV2.snapshot != nil && e.historyV2Contains(e.replicationV2.snapshotBase)
	if args[2] != "" && (!valid || args[2] != e.replicationV2.snapshotID) {
		frame.Pending = true
		return encodeReplicationFrame(frame) // client discards its stale transfer cursor
	}
	if !valid {
		e.closeReplicationSnapshot()
		e.replicationV2.snapshotRequested = true
		// After repeated failures the snapshot's rewrite waits as an
		// automatic one does, so a waiting replica cannot drive a failing
		// disk round a loop; any rewrite that finishes meanwhile serves it.
		if !e.RewriteActive() && e.snapshotRewriteAllowed() {
			if err := e.startSnapshotRewrite(); err != nil {
				return e.encode(fmt.Errorf("ERR preparing replication snapshot: %w", err), false)
			}
		}
		frame.Pending = true
		return encodeReplicationFrame(frame)
	}
	if e.replicationV2.snapshotBytes > maxReplicationSnapshotBytes {
		return e.encode(errors.New("ERR replication snapshot exceeds 1 GiB"), false)
	}
	if args[2] == "" && part != 0 {
		return e.encode(errSyntax, false)
	}
	if part > uint64(e.replicationV2.snapshotBytes) {
		return e.encode(errors.New("ERR invalid snapshot position"), false)
	}
	n := min(int64(replicationChunkBytes), e.replicationV2.snapshotBytes-int64(part))
	frame.Body = make([]byte, n)
	if n > 0 {
		got, err := e.replicationV2.snapshot.ReadAt(frame.Body, int64(part))
		if err != nil && err != io.EOF {
			return e.encode(err, false)
		}
		if got != int(n) {
			return e.encode(io.ErrUnexpectedEOF, false)
		}
	}
	frame.To = e.replicationV2.snapshotBase
	frame.SnapshotID = e.replicationV2.snapshotID
	frame.SnapshotOffset = part
	frame.SnapshotBytes = uint64(e.replicationV2.snapshotBytes)
	frame.SnapshotDone = part+uint64(n) == frame.SnapshotBytes
	return encodeReplicationFrame(frame)
}
