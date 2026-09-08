# Request allocation admission

Status: candidate, September 7, 2026. This extends covered command-reply
reservations to the event-loop TCP input phase; it is not a process-wide heap cap.

Before reading, the owner snapshots retained client buffers, AOF retention and
reply-arena capacity. Every I/O reader shares one atomic allowance: the smaller
of remaining 256 MiB aggregate space and 192 MiB input-class space. Readers
reserve a complete new backing allocation before growing a connection buffer,
including overlap with its old buffer. Complete RESP commands are validated
before reserving their command, argument strings and metadata; incomplete
commands do not copy already-received large arguments on every read. Argument
slices use their exact length. The parser preserves the reference decoder's
incomplete/protocol-error distinction and owns its decoded bytes.

Charges add conservative allocator headroom and accumulate through the read
phase, including temporary copies. After every reader joins, the event loop
accounts all decoded clients before executing the first command. This ensures
reply admission sees input decoded for later clients. Joined reservations are
then replaced by retained ownership. Failed admission discards that connection's
unexecuted parsed batch and buffered suffix, attempts a fixed error reply, and
closes the connection. Earlier executed batches remain executed; clients must
not blindly retry non-idempotent pipelines. If even the error cannot fit the
existing retained limits, the connection closes directly.

`INFO clients` adds `request_allocation_refusals` and
`request_allocation_peak_bytes`. Peak is the greatest starting retained charge
plus admitted input allocations in a joined phase; it is neither measured live
heap nor RSS. The refusal count is cumulative. Small fixed scratch areas and
transport bookkeeping, store growth, unmodeled persistence/replication/rewrite
allocations, compaction overlap, allocator metadata and kernel socket buffers
remain outside this contract. Refusing allocation does not recover Go heap that
the runtime has not yet reclaimed.

Brief guarded local checks pass incomplete-frame and refusal allocation probes,
parser differential seeds, owned-string checks and shared four-thread reader
admission/recovery. Hosted full tests, race detection, extended differential
fuzzing, incomplete-writer pressure and matched ordinary workloads remain
required. No throughput improvement or final-runtime soak pass is claimed.
