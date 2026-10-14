package core

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// INFO's Replication section, in Redis's terms first and Keel's own after
// (docs/info-compatibility.md, "Replication"): what Redis 8.10.1's
// genRedisInfoString writes, from the engine's replication state and what the
// transport knows of its links, and then Keel's own lines, unchanged, which
// Keel's tools read.

// ReplicationTransport is what the transport driving an engine knows of
// replication, which only it can see: a replica's link to its primary, and
// the replicas connected to a primary.
type ReplicationTransport struct {
	Link     ReplicationLink
	Replicas []ConnectedReplica
}

// ReplicationLink is a replica's link to its primary, as its transport sees
// it. Up is whether the connection is open and its last pull succeeded.
// UpSince is when it last came up, DownSince when it last went down, zero if
// it has never been up, and LastIO the last successful pull. Attempts are
// the connections tried since the link was last up and TotalAttempts all of
// them; Disconnected is how long it was down in the outages that have ended,
// counted from when it first came up.
type ReplicationLink struct {
	Up                         bool
	UpSince, DownSince, LastIO time.Time
	Attempts, TotalAttempts    int64
	Disconnected               time.Duration
}

// ConnectedReplica is one replica connected to a primary: its connection's
// address, the offset its last accepted pull started from, and when.
type ConnectedReplica struct {
	IP     string
	Port   int
	Offset uint64
	Pulled time.Time
}

// SetReplicationTransport installs what INFO reads the transport's
// replication from, or removes it when f is nil.
func (e *Engine) SetReplicationTransport(f func() ReplicationTransport) { e.replicationTransport = f }

// noReplicationID is Redis's master_replid2 when there is no previous
// replication ID: forty zeros.
const noReplicationID = "0000000000000000000000000000000000000000"

func (e *Engine) replicationInfo(b *strings.Builder) {
	var t ReplicationTransport
	if e.replicationTransport != nil {
		t = e.replicationTransport()
	}
	now := time.Now()
	offset, history := e.replication.offset, e.replication.bytes
	if e.replicationProtocol() == 2 {
		offset, history = e.replicationV2.end, e.replicationV2.bytes
	}

	b.WriteString("# Replication\r\n")
	replica := e.replicaOf() != ""
	if replica {
		b.WriteString("role:slave\r\n")
		e.replicaLinkInfo(b, t.Link, now)
	} else {
		b.WriteString("role:master\r\n")
	}
	fmt.Fprintf(b, "connected_slaves:%d\r\n", len(t.Replicas))
	for i, r := range t.Replicas {
		fmt.Fprintf(b, "slave%d:ip=%s,port=%d,state=online,offset=%d,lag=%d,io-thread=0\r\n",
			i, r.IP, r.Port, r.Offset, int64(now.Sub(r.Pulled)/time.Second))
	}
	// A replica reports the stream it follows, as a Redis replica takes its
	// master's replication ID and offset.
	replid, replOffset := e.replication.epoch, offset
	if replica {
		replOffset = e.replicaOffset
		if e.replicaEpoch != "" {
			replid = e.replicaEpoch
		}
	}
	fmt.Fprintf(b, "master_failover_state:no-failover\r\nmaster_replid:%s\r\nmaster_replid2:%s\r\nmaster_repl_offset:%d\r\nsecond_repl_offset:-1\r\n",
		replid, noReplicationID, replOffset)
	active := 0
	if e.feedsReplicas() {
		active = 1
	}
	fmt.Fprintf(b, "repl_backlog_active:%d\r\nrepl_backlog_size:%d\r\nrepl_backlog_first_byte_offset:%d\r\nrepl_backlog_histlen:%d\r\n",
		active, replicationHistoryLimit, e.historyFirstOffset(), history)

	// Keel's own, which its tools and tests read.
	ready := 0
	if e.replicaReady {
		ready = 1
	}
	age := int64(-1)
	if !e.replicaUpdated.IsZero() {
		age = time.Since(e.replicaUpdated).Milliseconds()
	}
	fmt.Fprintf(b, "primary_epoch:%s\r\nreplica_epoch:%s\r\nreplication_pending_keys:%d\r\nreplication_epoch_invalidated:%t\r\n", e.replication.epoch, e.replicaEpoch, len(e.replication.dirty), e.replication.invalidated)
	if e.replicationProtocol() == 2 {
		fmt.Fprintf(b, "replication_snapshot_bytes:%d\r\nreplica_checkpoint_resumed:%t\r\nreplica_snapshot_received:%d\r\n", e.replicationV2.snapshotBytes, e.replicaV2.resumed, e.replicaV2.snapshotReceived)
		// What the primary knows about its replicas. Lag is the distance
		// a promotion would lose right now; age says whether replication
		// is alive at all. Neither is a quorum signal - see
		// replication_ack.go for why.
		ackOffset, ackBehind, ackAge := e.ReplicationAcknowledged()
		fmt.Fprintf(b, "replication_acked_offset:%d\r\nreplication_lag_bytes:%d\r\nreplication_acked_age_ms:%d\r\n", ackOffset, ackBehind, ackAge)
	}
	fmt.Fprintf(b, "failover_term:%d\r\nfailover_held_term:%d\r\nfailover_fenced:%t\r\nwritable:%t\r\n", e.CurrentTerm(), e.failover.held, e.failover.fenced, e.writable())
	fmt.Fprintf(b, "replication_protocol:%d\r\nreplica_ready:%d\r\nreplica_offset:%d\r\nreplica_last_update_ms:%d\r\nprimary_offset:%d\r\nreplication_history_bytes:%d\r\n\r\n", e.replicationProtocol(), ready, e.replicaOffset, age, offset, history)
}

// replicaLinkInfo writes what a Redis replica writes of its master link,
// from the transport's view of e's link to its primary.
func (e *Engine) replicaLinkInfo(b *strings.Builder, link ReplicationLink, now time.Time) {
	host, portText, err := net.SplitHostPort(e.replicaOf())
	if err != nil {
		host, portText = e.replicaOf(), "0"
	}
	port, _ := strconv.Atoi(portText)
	status, lastIO := "down", -1
	if link.Up {
		status, lastIO = "up", int(now.Sub(link.LastIO)/time.Second)
	}
	syncing := 0
	if link.Up && !e.replicaReady {
		syncing = 1
	}
	fmt.Fprintf(b, "master_host:%s\r\nmaster_port:%d\r\nmaster_link_status:%s\r\nmaster_last_io_seconds_ago:%d\r\nmaster_sync_in_progress:%d\r\n",
		host, port, status, lastIO, syncing)
	// Keel applies a pull whole, so what a replica has read of the stream and
	// what it has applied are one offset; and it applies a full sync as it
	// arrives, keeping no buffer for one.
	fmt.Fprintf(b, "slave_read_repl_offset:%d\r\nslave_repl_offset:%d\r\nreplica_full_sync_buffer_size:0\r\nreplica_full_sync_buffer_peak:0\r\n",
		e.replicaOffset, e.replicaOffset)
	fmt.Fprintf(b, "master_current_sync_attempts:%d\r\nmaster_total_sync_attempts:%d\r\n", link.Attempts, link.TotalAttempts)
	disconnected := link.Disconnected
	if link.Up {
		fmt.Fprintf(b, "master_link_up_since_seconds:%d\r\nmaster_client_io_thread:0\r\n", int64(now.Sub(link.UpSince)/time.Second))
	} else {
		down := int64(-1)
		if !link.DownSince.IsZero() {
			down = int64(now.Sub(link.DownSince) / time.Second)
			disconnected += now.Sub(link.DownSince)
		}
		fmt.Fprintf(b, "master_link_down_since_seconds:%d\r\n", down)
	}
	fmt.Fprintf(b, "total_disconnect_time_sec:%d\r\nslave_read_only:1\r\n", int64(disconnected/time.Second))
}

// historyFirstOffset is repl_backlog_first_byte_offset: the offset the
// primary's history starts from, the first batch's under protocol 1 and the
// first byte's under protocol 2; 0 with no history.
func (e *Engine) historyFirstOffset() uint64 {
	if e.replicationProtocol() == 2 {
		if len(e.replicationV2.history) == 0 {
			return 0
		}
		return e.replicationV2.history[0].from + 1
	}
	if len(e.replication.history) == 0 {
		return 0
	}
	return e.replication.history[0].offset
}
