package core

import "time"

// Track the furthest received stream cursor reported by a valid delta pull.
// The next cursor can include a partial command buffered by the replica, so this
// is transport progress, not an applied or durable prefix. Its timestamp belongs
// to that greatest cursor; a lower cursor cannot make older progress look fresh.
// No per-replica identity is tracked, so these fields cannot establish quorum,
// current replica availability, promotion safety or acknowledged-write loss.
var replicaAck struct {
	offset uint64
	at     time.Time
}

func resetReplicaAcknowledgement() {
	replicaAck.offset = 0
	replicaAck.at = time.Time{}
}

// noteReplicaAcknowledged records a validated received cursor from this epoch.
func noteReplicaAcknowledged(offset uint64) {
	if replicaAck.at.IsZero() || offset >= replicaAck.offset {
		replicaAck.offset = offset
		replicaAck.at = time.Now()
	}
}

// ReplicationAcknowledged retains the INFO field names for compatibility. It
// reports the greatest validated received cursor, its distance from the current
// stream end and the age of its last confirmation. Age is negative when unknown.
func ReplicationAcknowledged() (offset, behind uint64, ageMs int64) {
	if replicaAck.at.IsZero() {
		return 0, 0, -1
	}
	if replicationV2.end > replicaAck.offset {
		behind = replicationV2.end - replicaAck.offset
	}
	return replicaAck.offset, behind, time.Since(replicaAck.at).Milliseconds()
}
