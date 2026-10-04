package core

import (
	"fmt"
	"strings"
	"time"

	"github.com/brandopakel/keel/internal/config"
	"github.com/brandopakel/keel/internal/data_structure"
)

// cmdMEMORY implements the MEMORY subcommands.
//
// USAGE reports one key and STATS reports estimated memory by type. It reports what the accounting believes one key
// costs, which is an estimate - see entryBytes - so it is useful for comparing
// keys against each other and for understanding why eviction chose what it did.
//
// The subcommand and its count have been checked against the command table by
// the time this runs; see containerCommands.
func (e *Engine) cmdMEMORY(args []string) []byte {
	if err := CommandError(&Command{Cmd: "MEMORY", Args: args}); err != nil {
		return Encode(err, false)
	}
	switch strings.ToUpper(args[0]) {
	case "STATS":
		out := ReplyMap{"keyspace.bytes", int64(e.space.TotalMemUsed()), "keys.count", int64(e.space.TotalKeys()), "keys.expires", int64(e.KeysWithExpiry())}
		e.space.EachKeyspace(func(ks data_structure.Keyspace) { out = append(out, ks.KeyspaceName()+".bytes", int64(ks.MemUsed())) })
		return Encode(out, false)
	case "USAGE":
		// SAMPLES is Redis's, and read as Redis reads it; the estimate here
		// is not sampled, so its count changes nothing.
		for i := 2; i < len(args); i += 2 {
			if !strings.EqualFold(args[i], "SAMPLES") || i+1 == len(args) {
				return Encode(errSyntax, false)
			}
			samples, valid := counterInteger(args[i+1])
			if !valid {
				return Encode(errNotAnInteger, false)
			}
			if samples < 0 {
				return Encode(errSyntax, false)
			}
		}
		bytes, exists := e.entryBytesAnywhere(args[1])
		if !exists {
			return nullReply()
		}
		return Encode(int64(bytes), false)
	}
	return memoryHelp
}

// memoryHelp is Redis's MEMORY HELP, for the subcommands this server has.
var memoryHelp = HelpReply("MEMORY",
	"STATS",
	"    Return information about the memory usage of the server.",
	"USAGE <key> [SAMPLES <count>]",
	"    Return memory in bytes used by <key> and its value, as this server",
	"    estimates it. SAMPLES is accepted, and the estimate is not sampled.")

// entryBytesAnywhere finds a key in whichever keyspace holds it.
//
// Keys live in a separate map per type here, so a name can be a string in one
// and a sorted set in another; the first match wins, which is the same order
// the command tables resolve in. Looking only in the string dictionary - as
// this did at first - reported nil for every set, sketch and filter.
func (e *Engine) entryBytesAnywhere(key string) (uint64, bool) {
	if owner, ok := e.space.OwnerOf(key); ok {
		return owner.EntryBytes(key)
	}
	return 0, false
}

// humanBytes renders a byte count the way redis-cli's INFO output does.
func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f%c", float64(n)/float64(div), "KMGTPE"[exp])
}

func evictionPolicyName() string {
	switch config.EvictStrategy {
	case config.LRU:
		return "allkeys-lru"
	case config.LFU:
		return "allkeys-lfu"
	default:
		return "allkeys-random"
	}
}

// ClientBufferStats describes retained event-loop buffers, not process RSS or
// kernel socket buffers. The event loop owns both collection and observation.
type ClientBufferStats struct {
	Connected, InputBytes, ReplyBytes, TotalBytes int
	RequestAllocationPeak                         int64
	RequestAllocationRefusals                     uint64
	// Connections the event loop closed for holding work past its timeout:
	// slow readers and half-sent requests; requests the server parsed and
	// never answered; and requests it never read at all, the socket still
	// holding their bytes. RunsUnreplied counts connections closed because a
	// run executed commands and produced no reply. All but the first should
	// stay at zero.
	ClosedSlow, ClosedUnanswered, ClosedUnread uint64
	RunsUnreplied                              uint64
	// ConnectionsReceived counts every connection accepted since startup.
	ConnectionsReceived uint64
}

// ClientBuffers is installed before accepting event-loop clients and removed
// after they close. Core-only and alternate transports leave it nil.
var ClientBuffers func() ClientBufferStats

// RedisCompatibleVersion is the Redis release whose command forms this server's
// subset follows - EXPIRE NX/XX/GT/LT, SET GET/EXAT/PXAT, LCS - and the version
// clients are told in INFO and HELLO. Libraries gate features on it: Rails'
// cache store reads redis_version before choosing EXPIRE NX, and fails without
// one. It is not a claim to implement all of Redis 7.0; the README's integration
// contract is that.
const RedisCompatibleVersion = "7.0.0"

// cmdINFO reports server state, in the section format redis-cli expects.
//
// It exists because eviction is otherwise invisible: without used_memory and
// evicted_keys there is no way to tell a cache that is working from one that is
// thrashing.
//
// The text is a verbatim string in RESP3, as Redis sends it. resp_version is
// the protocol of the connection asking, the same as HELLO's proto: 2, or 3
// after HELLO 3.
//
// Any number of sections may be named, as Redis allows; a name that is not a
// section adds nothing, and all, default and everything are every section.
func (e *Engine) cmdINFO(args []string) []byte {
	sections := make(map[string]bool, len(args))
	for _, arg := range args {
		sections[strings.ToLower(arg)] = true
	}
	every := len(args) == 0 || sections["all"] || sections["default"] || sections["everything"]

	var b strings.Builder
	want := func(name string) bool { return every || sections[name] }
	if want("clients") && ClientBuffers != nil {
		stats := ClientBuffers()
		fmt.Fprintf(&b, "# Clients\r\nconnected_clients:%d\r\nretained_input_bytes:%d\r\nretained_reply_bytes:%d\r\nretained_client_bytes:%d\r\n", stats.Connected, stats.InputBytes, stats.ReplyBytes, stats.TotalBytes)
		fmt.Fprintf(&b, "request_allocation_peak_bytes:%d\r\nrequest_allocation_refusals:%d\r\n", stats.RequestAllocationPeak, stats.RequestAllocationRefusals)
		fmt.Fprintf(&b, "clients_closed_slow:%d\r\nclients_closed_unanswered:%d\r\nclients_closed_unread:%d\r\nclients_closed_unreplied:%d\r\n\r\n",
			stats.ClosedSlow, stats.ClosedUnanswered, stats.ClosedUnread, stats.RunsUnreplied)
	}
	if want("clients") && CommandAllocations != nil {
		stats := CommandAllocations
		fmt.Fprintf(&b, "command_allocation_limit_bytes:%d\r\ncommand_allocation_reserved_bytes:%d\r\ncommand_allocation_peak_bytes:%d\r\ncommand_allocation_refusals:%d\r\n", stats.Limit, stats.Reserved, stats.Peak, stats.Refusals)
	}

	if want("replication") {
		role := "primary"
		if config.ReplicaOf != "" {
			role = "replica"
		}
		ready := 0
		if replicaReady {
			ready = 1
		}
		age := int64(-1)
		if !replicaUpdated.IsZero() {
			age = time.Since(replicaUpdated).Milliseconds()
		}
		fmt.Fprintf(&b, "# Replication\r\nprimary_epoch:%s\r\nreplica_epoch:%s\r\nreplication_pending_keys:%d\r\nreplication_epoch_invalidated:%t\r\n", replication.epoch, replicaEpoch, len(replication.dirty), replication.invalidated)
		offset, history := replication.offset, replication.bytes
		if config.ReplicationProtocol == 2 {
			offset, history = replicationV2.end, replicationV2.bytes
			fmt.Fprintf(&b, "replication_snapshot_bytes:%d\r\nreplica_checkpoint_resumed:%t\r\nreplica_snapshot_received:%d\r\n", replicationV2.snapshotBytes, replicaV2.resumed, replicaV2.snapshotReceived)
			// What the primary knows about its replicas. Lag is the distance
			// a promotion would lose right now; age says whether replication
			// is alive at all. Neither is a quorum signal - see
			// replication_ack.go for why.
			ackOffset, ackBehind, ackAge := ReplicationAcknowledged()
			fmt.Fprintf(&b, "replication_acked_offset:%d\r\nreplication_lag_bytes:%d\r\nreplication_acked_age_ms:%d\r\n", ackOffset, ackBehind, ackAge)
		}
		fmt.Fprintf(&b, "failover_term:%d\r\nfailover_held_term:%d\r\nfailover_fenced:%t\r\nwritable:%t\r\n", CurrentTerm(), HeldTerm(), Fenced(), Writable())
		fmt.Fprintf(&b, "replication_protocol:%d\r\nrole:%s\r\nreplica_ready:%d\r\nreplica_offset:%d\r\nreplica_last_update_ms:%d\r\nprimary_offset:%d\r\nreplication_history_bytes:%d\r\n\r\n", config.ReplicationProtocol, role, ready, replicaOffset, age, offset, history)
	}
	if want("server") {
		fmt.Fprintf(&b, "# Server\r\nkeel_version:%s\r\nredis_version:%s\r\nredis_mode:standalone\r\nresp_version:%d\r\n\r\n",
			config.BuildVersion(), RedisCompatibleVersion, replyProtocol())
	}
	if want("memory") {
		used := e.space.TotalMemUsed()
		fmt.Fprintf(&b, "# Memory\r\nused_memory:%d\r\nused_memory_human:%s\r\n",
			used, humanBytes(used))
		fmt.Fprintf(&b, "maxmemory:%d\r\nmaxmemory_human:%s\r\nmaxmemory_policy:%s\r\n\r\n",
			config.MaxMemory, humanBytes(config.MaxMemory), evictionPolicyName())
	}
	if want("stats") {
		fmt.Fprintf(&b, "# Stats\r\nevicted_keys:%d\r\nexpired_keys:%d\r\n",
			e.space.Evicted(), e.ExpiredKeys())
		if ClientBuffers != nil {
			fmt.Fprintf(&b, "total_connections_received:%d\r\n", ClientBuffers().ConnectionsReceived)
		}
		b.WriteString("\r\n")
	}
	if want("persistence") {
		base, current, rewrites, keys := AOFStats()
		enabled := 0
		if AOFEnabled() {
			enabled = 1
		}
		fmt.Fprintf(&b, "# Persistence\r\naof_enabled:%d\r\naof_base_size:%d\r\naof_current_size:%d\r\n",
			enabled, base, current)
		active := 0
		if RewriteActive() {
			active = 1
		}
		status := "ok"
		if aof.failed != nil {
			status = "err"
		}
		fmt.Fprintf(&b, "aof_rewrite_in_progress:%d\r\naof_last_write_status:%s\r\naof_buffer_length:%d\r\n", active, status, len(aof.buf))
		rewriteStatusInfo(&b)
		pending := 0
		if aof.syncPending != nil {
			pending = 1
		}
		encoded, written, synced, ready := AOFPositions()
		fmt.Fprintf(&b, "aof_encoded_offset:%d\r\naof_appended_offset:%d\r\naof_synced_offset:%d\r\naof_reply_offset:%d\r\n", encoded, written, synced, ready)
		fmt.Fprintf(&b, "aof_pending_fsync:%d\r\naof_pending_append_bytes:%d\r\n", pending, appendBytes)
		fmt.Fprintf(&b, "aof_rewrite_dirty_keys:%d\r\naof_rewrite_dirty_bytes:%d\r\naof_rewrite_budget_aborts:%d\r\n", len(rewrite.dirty), rewrite.dirtyBytes, rewriteBudgetAborts)
		rewritePending := 0
		if pendingRewriteIO != nil && pendingRewriteIO.body == nil {
			rewritePending = 1
		}
		fmt.Fprintf(&b, "aof_rewrite_pending_sync:%d\r\n", rewritePending)
		pendingWriteBytes := 0
		if pendingRewriteIO != nil {
			pendingWriteBytes = cap(pendingRewriteIO.body)
		}
		fmt.Fprintf(&b, "aof_rewrite_pending_write_bytes:%d\r\n", pendingWriteBytes)
		persistenceIOInfo(&b)
		fmt.Fprintf(&b, "aof_rewrites:%d\r\naof_keys_at_last_rewrite:%d\r\n\r\n", rewrites, keys)
	}
	if want("keyspace") {
		fmt.Fprintf(&b, "# Keyspace\r\ndb0:keys=%d,expires=%d\r\n\r\n",
			e.space.TotalKeys(), e.KeysWithExpiry())
	}

	return Encode(ReplyVerbatim(b.String()), false)
}

// cmdBGREWRITEAOF starts rewriting the append-only file and returns at once.
//
// It only starts it. The walk then advances a slice per event-loop cycle, so
// the reply arrives long before the work is finished and the server keeps
// answering throughout - which is what the "BG" in the name has always meant to
// every client and runbook that calls it.
//
// Redis gets that by forking; a Go runtime does not survive a bare fork, so
// here it comes from slicing the walk instead. INFO persistence reports
// aof_rewrites, which is how a caller waits for one to finish, and
// aof_last_bgrewrite_status, which says whether the last one did. A rewrite
// that fails leaves the log as it was and the server serving; see
// aof_rewrite_status.go, which also has the replies.
func (e *Engine) cmdBGREWRITEAOF(args []string) []byte {
	if len(args) != 0 {
		return Encode(wrongArguments("BGREWRITEAOF"), false)
	}
	return bgRewriteAOF()
}
