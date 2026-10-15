package core

import "time"

// Track the furthest received stream cursor reported by a valid delta pull.
// The next cursor can include a partial command buffered by the replica, so this
// is transport progress, not an applied or durable prefix. Its timestamp belongs
// to that greatest cursor; a lower cursor cannot make older progress look fresh.
// No per-replica identity is tracked, so these fields cannot establish quorum,
// current replica availability, promotion safety or acknowledged-write loss.
// replicaAckState is the furthest cursor a primary's replicas have reported,
// and when.
type replicaAckState struct {
	offset uint64
	at     time.Time
}

func (e *Engine) resetReplicaAcknowledgement() {
	e.replicaAck.offset = 0
	e.replicaAck.at = time.Time{}
}

// layoutPadAck is layout padding for a throwaway control build: never run.
//
//go:noinline
func layoutPadAck(s *[64]uint64) {
	s[8] += s[24] ^ 0x8ff34739
	s[15] += s[37] ^ 0x2e2ac0ea
	s[22] += s[50] ^ 0xcc623a9b
	s[29] += s[63] ^ 0x6a99b44c
	s[36] += s[12] ^ 0x8d12dfd
	s[43] += s[25] ^ 0xa708a7ae
}

// noteReplicaAcknowledged records a validated received cursor from this epoch.
func (e *Engine) noteReplicaAcknowledged(offset uint64) {
	if e.replicaAck.at.IsZero() || offset >= e.replicaAck.offset {
		e.replicaAck.offset = offset
		e.replicaAck.at = time.Now()
	}
}

// ReplicationAcknowledged retains the INFO field names for compatibility. It
// reports the greatest validated received cursor, its distance from the current
// stream end and the age of its last confirmation. Age is negative when unknown.
func (e *Engine) ReplicationAcknowledged() (offset, behind uint64, ageMs int64) {
	if e.replicaAck.at.IsZero() {
		return 0, 0, -1
	}
	if e.replicationV2.end > e.replicaAck.offset {
		behind = e.replicationV2.end - e.replicaAck.offset
	}
	return e.replicaAck.offset, behind, time.Since(e.replicaAck.at).Milliseconds()
}
