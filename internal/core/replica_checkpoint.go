package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/brandopakel/keel/internal/config"
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

var checkpointRename = os.Rename
var checkpointSyncDir = syncDir
var checkpointSync = func(f *os.File) error { return f.Sync() }

func openAOFDigest(path string) error {
	aof.digest = nil
	aof.digestBytes = 0
	if config.ReplicaOf == "" || config.ReplicationProtocol != 2 {
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
	aof.digest = h
	aof.digestBytes = n
	return nil
}
func recordAOFDigest(body []byte) {
	if aof.digest != nil {
		aof.digest.Write(body)
		aof.digestBytes += int64(len(body))
	}
}
func currentAOFDigest() string {
	if aof.digest == nil {
		return ""
	}
	return hex.EncodeToString(aof.digest.Sum(nil))
}

func loadReplicaCheckpoint() error {
	// Missing or invalid checkpoints are a full-sync fallback, not a writable
	// or readable partially trusted state.
	if aof.path == "" || aof.digest == nil || len(aof.buf) != 0 {
		return nil
	}
	f, err := os.Open(aof.path + ".replica-checkpoint")
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
	if len(body) > 4096 || json.Unmarshal(body, &cp) != nil || cp.Version != 2 || cp.Primary != config.ReplicaOf || len(cp.Epoch) != 32 || cp.Bytes != aof.digestBytes || cp.SHA256 != currentAOFDigest() {
		return nil
	}
	if _, err := hex.DecodeString(cp.Epoch); err != nil {
		return nil
	}
	replicaEpoch, replicaOffset = cp.Epoch, cp.Offset
	replicaV2.trusted = true
	replicaV2.resumed = true
	replicaV2.checkpoint = cp
	return nil
}

func saveReplicaCheckpoint() error {
	if aof.digest == nil || aof.file == nil {
		return errors.New("replication checkpoint requires a local AOF digest")
	}
	// Replica checkpoints strengthen local persistence even under everysec/no:
	// the state must be synced before the checkpoint can name it as resumable.
	if err := flushAOF(true); err != nil {
		return err
	}
	cp := replicaCheckpoint{Version: 2, Primary: config.ReplicaOf, Epoch: replicaEpoch, Offset: replicaOffset, Bytes: aof.digestBytes, SHA256: currentAOFDigest()}
	if cp == replicaV2.checkpoint {
		return nil
	}
	body, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(aof.path), ".keel-replica-checkpoint-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(body); err == nil {
		err = checkpointSync(f)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = checkpointRename(tmp, aof.path+".replica-checkpoint"); err != nil {
		return err
	}
	if err = checkpointSyncDir(filepath.Dir(aof.path)); err != nil {
		return err
	}
	replicaV2.checkpoint = cp
	return nil
}

// Reads replica cursor state that applyReplicationV2 mutates on the command
// thread. Callers must invoke this before the replica transport worker starts.
func ReplicaResumeCursor() (string, uint64) {
	if config.ReplicationProtocol == 2 && replicaV2.trusted {
		return replicaEpoch, replicaOffset
	}
	return "", 0
}
