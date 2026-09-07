package core

import "time"

// What a primary knows about its replicas, which until now was nothing.
//
// The acknowledgement is already on the wire and was being thrown away. A
// replica asks to resume from an offset, and asking for it is a statement that
// everything before it has been applied and made durable locally. Recording the
// furthest such offset costs nothing and needs no protocol change.
//
// What this is: a liveness and lag signal. Replication is working if the
// acknowledged offset advances, and the distance behind the primary's own
// offset is how much a promotion would lose right now.
//
// What this is not: a durability guarantee, and it must not be read as one.
// With more than one replica this records the furthest any of them has reached,
// not the nearest. A WAIT that promised "n replicas hold this write" needs the
// opposite - per-replica offsets and a count of those at or past a given one -
// and that needs connection identity the command layer does not currently have.
// See docs/replication-alpha.md.
var replicaAck struct {
	offset uint64
	at     time.Time
}

func resetReplicaAcknowledgement() {
	replicaAck.offset = 0
	replicaAck.at = time.Time{}
}

// noteReplicaAcknowledged records a replica's confirmed prefix. Only called for
// a pull in this primary's own epoch: an offset from another history names a
// position in a stream this primary never produced.
func noteReplicaAcknowledged(offset uint64) {
	replicaAck.at = time.Now()
	if offset > replicaAck.offset {
		replicaAck.offset = offset
	}
}

// ReplicationAcknowledged reports the furthest offset a replica has confirmed,
// how far that is behind what this primary has produced, and how long ago the
// confirmation arrived. Age is negative when nothing has ever acknowledged.
func ReplicationAcknowledged() (offset, behind uint64, ageMs int64) {
	if replicaAck.at.IsZero() {
		return 0, 0, -1
	}
	if replicationV2.end > replicaAck.offset {
		behind = replicationV2.end - replicaAck.offset
	}
	return replicaAck.offset, behind, time.Since(replicaAck.at).Milliseconds()
}
