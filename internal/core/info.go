package core

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/brandopakel/keel/internal/config"
	"github.com/brandopakel/keel/internal/procinfo"
)

// ServerInfo is what INFO reports of the transport serving an engine that the
// engine cannot know itself: where it listens and how it is set up. The event
// loop installs it with SetServerInfo before accepting clients.
type ServerInfo struct {
	Port, MaxClients, IOThreads int
	// Hz is how many times a second the transport runs its clock-driven work,
	// Redis's hz.
	Hz int
	// Multiplexer is the readiness facility the transport waits on: epoll or
	// kqueue.
	Multiplexer string
}

// SetServerInfo installs on e what INFO reports of the transport driving e,
// or removes it when info is nil, which leaves those fields out.
func (e *Engine) SetServerInfo(info *ServerInfo) { e.serverInfo = info }

// NoteMemoryPeak records the memory e's stores hold, if it is the most they
// have held, for INFO's used_memory_peak, and returns it. The event loop calls
// it once a turn and INFO calls it too, so the peak INFO reports is never below
// the memory it reports in use.
func (e *Engine) NoteMemoryPeak() uint64 {
	used := e.space.TotalMemUsed()
	if used > e.memoryPeak {
		e.memoryPeak, e.memoryPeakAt = used, time.Now()
	}
	return used
}

// cmdINFO reports server state, in the section format redis-cli expects.
//
// It exists because eviction is otherwise invisible: without used_memory and
// evicted_keys there is no way to tell a cache that is working from one that is
// thrashing. Beyond that it reports what monitoring built for Redis reads, under
// Redis's names, in Redis's order and units (docs/info-compatibility.md): a
// field Keel can compute with Redis's meaning, Redis's value for a feature not
// in use where that is true of Keel, and nothing it would have to make up.
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

	// The sections in the order Redis writes them.
	var b strings.Builder
	for _, s := range [...]struct {
		name  string
		write func(*Engine, *strings.Builder)
	}{
		{"server", (*Engine).serverInfoSection},
		{"clients", (*Engine).clientsInfo},
		{"memory", (*Engine).memoryInfo},
		{"persistence", (*Engine).persistenceInfo},
		{"stats", (*Engine).statsInfo},
		{"replication", (*Engine).replicationInfo},
		{"cpu", (*Engine).cpuInfo},
		{"cluster", (*Engine).clusterInfo},
		{"keyspace", (*Engine).keyspaceInfo},
	} {
		if every || sections[s.name] {
			s.write(e, &b)
		}
	}
	return e.encode(ReplyVerbatim(b.String()), false)
}

func (e *Engine) serverInfoSection(b *strings.Builder) {
	now := time.Now()
	up := now.Sub(e.started)
	s := e.serverInfo
	fmt.Fprintf(b, "# Server\r\nkeel_version:%s\r\nredis_version:%s\r\nredis_mode:standalone\r\nos:%s\r\narch_bits:%d\r\n",
		config.BuildVersion(), RedisCompatibleVersion, procinfo.OS(), strconv.IntSize)
	if s != nil {
		fmt.Fprintf(b, "multiplexing_api:%s\r\n", s.Multiplexer)
	}
	// Keel does not talk to systemd or upstart.
	fmt.Fprintf(b, "process_id:%d\r\nprocess_supervised:no\r\nrun_id:%s\r\n", os.Getpid(), e.runID)
	if s != nil {
		fmt.Fprintf(b, "tcp_port:%d\r\n", s.Port)
	}
	// Uptime counts from when e was made, which a server does at startup.
	fmt.Fprintf(b, "server_time_usec:%d\r\nuptime_in_seconds:%d\r\nuptime_in_days:%d\r\n",
		now.UnixMicro(), int64(up/time.Second), int64(up/(24*time.Hour)))
	if s != nil {
		fmt.Fprintf(b, "hz:%d\r\nconfigured_hz:%d\r\n", s.Hz, s.Hz)
	}
	if exe, err := os.Executable(); err == nil {
		fmt.Fprintf(b, "executable:%s\r\n", exe)
	}
	// Keel takes flags, not a configuration file, which Redis reports empty.
	b.WriteString("config_file:\r\n")
	if s != nil {
		active := 0
		if s.IOThreads > 1 {
			active = 1
		}
		fmt.Fprintf(b, "io_threads_active:%d\r\n", active)
	}
	fmt.Fprintf(b, "resp_version:%d\r\n\r\n", e.replyProtocol())
}

func (e *Engine) clientsInfo(b *strings.Builder) {
	if e.clientBuffers == nil && e.commandAllocations == nil {
		return
	}
	b.WriteString("# Clients\r\n")
	if e.clientBuffers != nil {
		stats := e.clientBuffers()
		fmt.Fprintf(b, "connected_clients:%d\r\ncluster_connections:0\r\n", stats.Connected)
		if s := e.serverInfo; s != nil {
			fmt.Fprintf(b, "maxclients:%d\r\n", s.MaxClients)
		}
		// Keel has no blocking commands, client tracking, Pub/Sub or WATCH (the
		// README's integration contract), so Redis's counts of them are zero.
		b.WriteString("blocked_clients:0\r\ntracking_clients:0\r\npubsub_clients:0\r\nwatching_clients:0\r\n" +
			"total_watched_keys:0\r\ntotal_blocking_keys:0\r\ntotal_blocking_keys_on_nokey:0\r\n")
		fmt.Fprintf(b, "retained_input_bytes:%d\r\nretained_reply_bytes:%d\r\nretained_client_bytes:%d\r\n", stats.InputBytes, stats.ReplyBytes, stats.TotalBytes)
		fmt.Fprintf(b, "request_allocation_peak_bytes:%d\r\nrequest_allocation_refusals:%d\r\n", stats.RequestAllocationPeak, stats.RequestAllocationRefusals)
		fmt.Fprintf(b, "clients_closed_slow:%d\r\nclients_closed_unanswered:%d\r\nclients_closed_unread:%d\r\nclients_closed_unreplied:%d\r\n",
			stats.ClosedSlow, stats.ClosedUnanswered, stats.ClosedUnread, stats.RunsUnreplied)
	}
	if stats := e.commandAllocations; stats != nil {
		fmt.Fprintf(b, "command_allocation_limit_bytes:%d\r\ncommand_allocation_reserved_bytes:%d\r\ncommand_allocation_peak_bytes:%d\r\ncommand_allocation_refusals:%d\r\n", stats.Limit, stats.Reserved, stats.Peak, stats.Refusals)
	}
	b.WriteString("\r\n")
}

func (e *Engine) memoryInfo(b *strings.Builder) {
	used := e.NoteMemoryPeak()
	fmt.Fprintf(b, "# Memory\r\nused_memory:%d\r\nused_memory_human:%s\r\n", used, humanBytes(used))
	if rss, ok := procinfo.ResidentBytes(); ok {
		fmt.Fprintf(b, "used_memory_rss:%d\r\nused_memory_rss_human:%s\r\n", rss, humanBytes(rss))
	}
	// Redis divides the memory in use by the peak; with nothing ever stored,
	// both are zero and the one is all of the other.
	peak, percent := e.memoryPeak, 100.0
	if peak > 0 {
		percent = float64(used) * 100 / float64(peak)
	}
	fmt.Fprintf(b, "used_memory_peak:%d\r\nused_memory_peak_human:%s\r\nused_memory_peak_time:%d\r\nused_memory_peak_perc:%.2f%%\r\n",
		peak, humanBytes(peak), e.memoryPeakAt.Unix(), percent)
	if total, ok := procinfo.PhysicalBytes(); ok {
		fmt.Fprintf(b, "total_system_memory:%d\r\ntotal_system_memory_human:%s\r\n", total, humanBytes(total))
	}
	limits := e.space.Limits()
	fmt.Fprintf(b, "maxmemory:%d\r\nmaxmemory_human:%s\r\nmaxmemory_policy:%s\r\n",
		limits.MaxMemory, humanBytes(limits.MaxMemory), evictionPolicyName(limits.Eviction))
	// Keel defragments nothing and frees nothing lazily.
	b.WriteString("active_defrag_running:0\r\nlazyfree_pending_objects:0\r\nlazyfreed_objects:0\r\n\r\n")
}

func (e *Engine) persistenceInfo(b *strings.Builder) {
	// Keel replays its log before it accepts a client, so INFO never runs
	// while it loads, and it takes no RDB snapshots.
	b.WriteString("# Persistence\r\nloading:0\r\nasync_loading:0\r\nrdb_bgsave_in_progress:0\r\nrdb_last_bgsave_status:ok\r\nrdb_saves:0\r\n")
	base, current, rewrites, keys := e.AOFStats()
	enabled := 0
	if e.AOFEnabled() {
		enabled = 1
	}
	fmt.Fprintf(b, "aof_enabled:%d\r\naof_base_size:%d\r\naof_current_size:%d\r\n",
		enabled, base, current)
	active := 0
	if e.RewriteActive() {
		active = 1
	}
	status := "ok"
	if e.aof.failed != nil || e.logRetrying() {
		status = "err"
	}
	fmt.Fprintf(b, "aof_rewrite_in_progress:%d\r\naof_last_write_status:%s\r\naof_buffer_length:%d\r\n", active, status, len(e.aof.buf))
	e.rewriteStatusInfo(b)
	pending := 0
	if e.aof.syncPending != nil {
		pending = 1
	}
	encoded, written, synced, ready := e.AOFPositions()
	fmt.Fprintf(b, "aof_encoded_offset:%d\r\naof_appended_offset:%d\r\naof_synced_offset:%d\r\naof_reply_offset:%d\r\n", encoded, written, synced, ready)
	fmt.Fprintf(b, "aof_pending_fsync:%d\r\naof_pending_append_bytes:%d\r\n", pending, e.appendBytes)
	fmt.Fprintf(b, "aof_rewrite_dirty_keys:%d\r\naof_rewrite_dirty_bytes:%d\r\naof_rewrite_budget_aborts:%d\r\n", len(e.rewrite.dirty), e.rewrite.dirtyBytes, e.rewriteBudgetAborts)
	rewritePending := 0
	if e.pendingRewriteIO != nil && e.pendingRewriteIO.body == nil {
		rewritePending = 1
	}
	fmt.Fprintf(b, "aof_rewrite_pending_sync:%d\r\n", rewritePending)
	pendingWriteBytes := 0
	if e.pendingRewriteIO != nil {
		pendingWriteBytes = cap(e.pendingRewriteIO.body)
	}
	fmt.Fprintf(b, "aof_rewrite_pending_write_bytes:%d\r\n", pendingWriteBytes)
	e.persistenceIOInfo(b)
	fmt.Fprintf(b, "aof_rewrites:%d\r\naof_keys_at_last_rewrite:%d\r\n\r\n", rewrites, keys)
}

func (e *Engine) statsInfo(b *strings.Builder) {
	b.WriteString("# Stats\r\n")
	if e.clientBuffers != nil {
		stats := e.clientBuffers()
		fmt.Fprintf(b, "total_connections_received:%d\r\nrejected_connections:%d\r\n", stats.ConnectionsReceived, stats.ConnectionsRejected)
	}
	fmt.Fprintf(b, "expired_keys:%d\r\nevicted_keys:%d\r\n", e.ExpiredKeys(), e.space.Evicted())
	// Keel evicts no clients and has no scripts, Pub/Sub or client tracking,
	// and it never forks.
	b.WriteString("evicted_clients:0\r\nevicted_scripts:0\r\npubsub_channels:0\r\npubsub_patterns:0\r\npubsubshard_channels:0\r\n" +
		"latest_fork_usec:0\r\ntotal_forks:0\r\ntracking_total_keys:0\r\ntracking_total_items:0\r\ntracking_total_prefixes:0\r\n\r\n")
}

func (e *Engine) replicationInfo(b *strings.Builder) {
	role := "primary"
	if e.replicaOf() != "" {
		role = "replica"
	}
	ready := 0
	if e.replicaReady {
		ready = 1
	}
	age := int64(-1)
	if !e.replicaUpdated.IsZero() {
		age = time.Since(e.replicaUpdated).Milliseconds()
	}
	fmt.Fprintf(b, "# Replication\r\nprimary_epoch:%s\r\nreplica_epoch:%s\r\nreplication_pending_keys:%d\r\nreplication_epoch_invalidated:%t\r\n", e.replication.epoch, e.replicaEpoch, len(e.replication.dirty), e.replication.invalidated)
	offset, history := e.replication.offset, e.replication.bytes
	if e.replicationProtocol() == 2 {
		offset, history = e.replicationV2.end, e.replicationV2.bytes
		fmt.Fprintf(b, "replication_snapshot_bytes:%d\r\nreplica_checkpoint_resumed:%t\r\nreplica_snapshot_received:%d\r\n", e.replicationV2.snapshotBytes, e.replicaV2.resumed, e.replicaV2.snapshotReceived)
		// What the primary knows about its replicas. Lag is the distance
		// a promotion would lose right now; age says whether replication
		// is alive at all. Neither is a quorum signal - see
		// replication_ack.go for why.
		ackOffset, ackBehind, ackAge := e.ReplicationAcknowledged()
		fmt.Fprintf(b, "replication_acked_offset:%d\r\nreplication_lag_bytes:%d\r\nreplication_acked_age_ms:%d\r\n", ackOffset, ackBehind, ackAge)
	}
	fmt.Fprintf(b, "failover_term:%d\r\nfailover_held_term:%d\r\nfailover_fenced:%t\r\nwritable:%t\r\n", e.CurrentTerm(), e.failover.held, e.failover.fenced, e.writable())
	fmt.Fprintf(b, "replication_protocol:%d\r\nrole:%s\r\nreplica_ready:%d\r\nreplica_offset:%d\r\nreplica_last_update_ms:%d\r\nprimary_offset:%d\r\nreplication_history_bytes:%d\r\n\r\n", e.replicationProtocol(), role, ready, e.replicaOffset, age, offset, history)
}

// cpuInfo reports the processor time the process has used, as Redis does from
// getrusage, in seconds with six decimals; where the platform cannot say,
// there is no CPU section.
func (e *Engine) cpuInfo(b *strings.Builder) {
	cpu, ok := procinfo.CPUTime()
	if !ok {
		return
	}
	fmt.Fprintf(b, "# CPU\r\nused_cpu_sys:%s\r\nused_cpu_user:%s\r\nused_cpu_sys_children:%s\r\nused_cpu_user_children:%s\r\n\r\n",
		cpuSeconds(cpu.Sys), cpuSeconds(cpu.User), cpuSeconds(cpu.ChildrenSys), cpuSeconds(cpu.ChildrenUser))
}

// cpuSeconds writes d as Redis writes processor time: seconds, a point and
// six digits of microseconds.
func cpuSeconds(d time.Duration) string {
	us := d.Microseconds()
	return fmt.Sprintf("%d.%06d", us/1e6, us%1e6)
}

func (e *Engine) clusterInfo(b *strings.Builder) {
	b.WriteString("# Cluster\r\ncluster_enabled:0\r\n\r\n")
}

func (e *Engine) keyspaceInfo(b *strings.Builder) {
	fmt.Fprintf(b, "# Keyspace\r\ndb0:keys=%d,expires=%d\r\n\r\n",
		e.space.TotalKeys(), e.KeysWithExpiry())
}
