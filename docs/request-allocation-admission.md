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

Unknown-command errors quote at most 128 bytes of the command name, followed
by `...` when truncated. This prevents formatting and CRLF sanitization from
copying an entire untrusted token after request admission ends. Ordinary unknown
command errors and their single-frame escaping remain unchanged.

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

Initial hosted diagnostics at `b456c27` pass 3,211,347 differential fuzz inputs
and twenty shared-reader race repetitions. Five parser benchmark samples show
166 to 150 B/op for a 64-byte SET, with five allocations in both implementations.
For an incomplete request with an already-received 1 MiB key, repeated decoding
falls from about 1,048,630 B/op to zero; the complete value has not arrived, so
neither parser accepts the command. These are parser diagnostics, not server
throughput. The timing samples use fixed function order and show warmup drift;
they are not an adoption gate. Evidence: `request-admission-hosted-initial-2026-09-07.json.gz`.

Further review reproduced two gaps. Expanding Unicode conversion allocated
3,421,448 bytes against a 1,310,912-byte charge; pre-sizing its one conversion
buffer now passes that reservation check. An unknown 1 MiB command name also
generated an oversized formatted/sanitized error; bounded quoting now passes a
64 KiB temporary-allocation ceiling. Both failures and corrections are retained
in `request-admission-unicode-2026-09-07.json.gz` and
`request-admission-errors-2026-09-07.json.gz`. They require final hosted checks
beyond the earlier `b456c27` results.

Matched comparison run 34177607787 was cancelled during setup after detecting
invalid selected scenario names. Corrected run 34177666365 compares twelve
unchanged workloads at `b456c27` against `39043e3`, five alternating pairs each;
it predates the Unicode/diagnostic follow-ups and is not final-runtime validation.
