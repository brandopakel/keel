package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// A checkpoint is valid only for the exact synced AOF image it names. An extra
// suffix, repaired/truncated prefix, different rewrite or damaged metadata makes
// restart request a full snapshot. There is never replay beyond a checkpoint
// followed by delta replay from an earlier state.
type replicaCheckpoint struct {
	Version int    `json:"version"`
	Primary string `json:"primary"`
	Epoch   string `json:"epoch"`
	Offset  uint64 `json:"offset"`
	Bytes   int64  `json:"aof_bytes"`
	SHA256  string `json:"aof_sha256"`
}

// syncFile is a checkpoint's sync, unless a test has replaced the engine's
// checkpointSync.
func syncFile(f *os.File) error { return f.Sync() }

func (e *Engine) openAOFDigest(path string) error {
	e.aof.digest = nil
	e.aof.digestBytes = 0
	if e.replicaOf() == "" || e.replicationProtocol() != 2 {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return err
	}
	e.aof.digest = h
	e.aof.digestBytes = n
	return nil
}
func (e *Engine) recordAOFDigest(body []byte) {
	if e.aof.digest != nil {
		e.aof.digest.Write(body)
		e.aof.digestBytes += int64(len(body))
	}
}
func (e *Engine) currentAOFDigest() string {
	if e.aof.digest == nil {
		return ""
	}
	return hex.EncodeToString(e.aof.digest.Sum(nil))
}

func (e *Engine) loadReplicaCheckpoint() error {
	// Missing or invalid checkpoints are a full-sync fallback, not a writable
	// or readable partially trusted state.
	if e.aof.path == "" || e.aof.digest == nil || len(e.aof.buf) != 0 {
		return nil
	}
	f, err := os.Open(e.aof.path + ".replica-checkpoint")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return err
	}
	var cp replicaCheckpoint
	if len(body) > 4096 || json.Unmarshal(body, &cp) != nil || cp.Version != 2 || cp.Primary != e.replicaOf() || len(cp.Epoch) != 32 || cp.Bytes != e.aof.digestBytes || cp.SHA256 != e.currentAOFDigest() {
		return nil
	}
	if _, err := hex.DecodeString(cp.Epoch); err != nil {
		return nil
	}
	e.replicaEpoch, e.replicaOffset = cp.Epoch, cp.Offset
	e.replicaV2.trusted = true
	e.replicaV2.resumed = true
	e.replicaV2.checkpoint = cp
	return nil
}

func (e *Engine) saveReplicaCheckpoint() error {
	if e.aof.digest == nil || e.aof.file == nil {
		return errors.New("replication checkpoint requires a local AOF digest")
	}
	// Replica checkpoints strengthen local persistence even under everysec/no:
	// the state must be synced before the checkpoint can name it as resumable.
	if err := e.flushAOF(true); err != nil {
		return err
	}
	cp := replicaCheckpoint{Version: 2, Primary: e.replicaOf(), Epoch: e.replicaEpoch, Offset: e.replicaOffset, Bytes: e.aof.digestBytes, SHA256: e.currentAOFDigest()}
	if cp == e.replicaV2.checkpoint {
		return nil
	}
	body, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(e.aof.path), ".keel-replica-checkpoint-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(body); err == nil {
		err = e.checkpointSync(f)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = e.checkpointRename(tmp, e.aof.path+".replica-checkpoint"); err != nil {
		return err
	}
	if err = e.checkpointSyncDir(filepath.Dir(e.aof.path)); err != nil {
		return err
	}
	e.replicaV2.checkpoint = cp
	return nil
}

// Reads replica cursor state that applyReplicationV2 mutates on the command
// thread. Callers must invoke this before the replica transport worker starts.
func (e *Engine) ReplicaResumeCursor() (string, uint64) {
	if e.replicationProtocol() == 2 && e.replicaV2.trusted {
		return e.replicaEpoch, e.replicaOffset
	}
	return "", 0
}

// layoutPad is part of a layout control, never merged: it is never called,
// and only moves the code after it by one 32-byte slot.
//
//go:noinline
func layoutPad() {}

// layoutPad2 is part of the same layout control, never merged: it moves the
// code after it by one more 32-byte slot.
//
//go:noinline
func layoutPad2() {}
