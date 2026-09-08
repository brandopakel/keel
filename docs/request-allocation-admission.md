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

A refused speculative buffer-growth request first falls back to already-owned
spare capacity, including a compactable parsed prefix. The refusal metric counts
denied reservations; it can increase even when that fallback completes the read.

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

That initial 120-arm comparison completed with no command/connection errors.
Median paired throughput ratios were 0.995 (small read), 0.984 (balanced),
0.989 (1 KiB read), 0.989 (small write), 0.966 (write pipeline), 1.012 (1 MiB
read), 0.985 (1 MiB write), 0.992 (hash), 0.989 (sorted set), 0.990 (queue) and
0.998 (TTL). Pipeline median p99 rose from 1.631 to 1.735 ms. Many-client ratio
was 0.986, but baseline repetition four had a generator CPU warning and is not
a clean capacity result. These results justify further cost reduction, not a
general speedup claim. Raw evidence: `request-admission-matched-initial-2026-09-07.tar.gz`.

Diagnostics at `1c8930c` pass 3,629,581 differential fuzz inputs and twenty worker
race repetitions. Full read ownership falls from 174 to 158 B for one SET and
11,665 to 10,641 B for a pipeline of 64, with allocation counts still 6 and 327,
respectively. Fixed-order component timing medians rise 7.3% and 6.4%; those
timings are diagnostic and do not replace matched server results. Paired CPU
profiles at that same runtime complete 36 instrumented arms in run 34178979147;
the parser remains roughly 21–29% of pipelined CPU samples. Evidence:
`request-admission-hosted-final-2026-09-07.json.gz` and
`request-admission-profiles-2026-09-07.tar.gz`. “Final” in the archive name records
the then-current diagnostic, before the subsequent spare-capacity correction.

Review corrected premature refusal when an incomplete bulk header requested a
larger speculative read despite having enough existing capacity for its actual
suffix. The corrected regression fails the old reader and passes the fallback.
Its first fixture accidentally provided a complete bulk header, so it never
exercised speculative growth; those two fixture failures remain recorded and are
not claimed as runtime reproductions. `request-admission-review-2026-09-07.json.gz`
preserves these checks alongside bounded failed-AOF archive tests.

The benchmark harness now distinguishes a requested shutdown wait budget from
an explicitly passed binary setting. Omitted binary flags are reported as
implicit, and the matched transcript workflow requires a supported non-default
grace (30 seconds by default). Failed AOF preservation caps compressed output
at 64 MiB and reserves free-space headroom. Complete archives are verified before
the source is pruned; incompressible/low-space cases retain the original on the
runner, store bounded prefix/tail evidence, and stop further benchmark arms.
Those partial samples cannot establish full replay and the unexported original
lasts only until runner teardown. This avoids silently discarding failure bytes.

The repeated matched comparison at `1c8930c` (run 34178562831, before command
coallocation) passes all 120 arms without generator CPU warnings. Median paired
throughput ratios are 1.001 (small read), 0.996 (balanced), 1.003 (1 KiB read),
0.998 (small write), 0.956 (write pipeline), 1.012 (1 MiB read), 1.003 (1 MiB
write), 0.993 (hash), 0.991 (sorted set), 0.994 (queue), 0.989 (TTL) and 0.989
(many clients). Pipeline median p99 is 0.775 to 0.791 ms. This is a different
public VM from the initial comparison; compare pairs within each run, not their
absolute numbers across hosts. Evidence: `request-admission-matched-pre-coallocation-2026-09-07.tar.gz`.

A follow-up coallocates common one/two-argument commands with their argument
headers. At `11268c3`, the SET parser retains 150 B/op while falling from five
allocations to four; single read ownership falls from six allocations to five,
and a 64-command pipeline from 327 to 263. Hosted diagnostics pass 3,989,255
differential fuzz inputs, twenty shared-worker race repetitions and the full
Go/platform/race suite. Fixed-order pipeline component timings are roughly
unchanged against the pre-admission baseline in this run; matched server
measurements remain required. That follow-up is now integrated in PR #66 with
the spare-capacity correction. Evidence: `coallocated-command-hosted-2026-09-07.json.gz`.

The dedicated admission workflow also runs three race-instrumented repetitions
of the incomplete-writer and slow-reader socket recovery fixtures. The complete
Linux/Mac suites continue to run them too; brief local `-short` checks skip
the aggregate socket-pressure fixture. Earlier dedicated diagnostic artifacts
contain the worker tests but not this newly added socket step.

The coallocated server comparison (run 34179665418, `11268c3` versus merged
`15ce1c0`) completed all 120 arms with zero connection errors or generator CPU
warnings. It used five alternating pairs of ten-second measurements with
persistence disabled and disjoint exposed physical-core groups on one public VM.
Median paired throughput ratios and median arm p99s are:

| Workload | Candidate/baseline throughput | Baseline → candidate p99 (ms) |
| --- | ---: | ---: |
| cache-balanced-64 | 0.995 | 0.351 → 0.351 |
| cache-read-1k | 0.999 | 0.391 → 0.391 |
| cache-read-1m | 0.994 | 5.439 → 5.983 |
| cache-read-64 | 0.996 | 0.343 → 0.351 |
| cache-ttl | 0.990 | 0.367 → 0.375 |
| cache-write-1m | 0.944 | 42.239 → 42.239 |
| cache-write-64 | 0.997 | 0.351 → 0.351 |
| cache-write-pipeline-64 | 0.991 | 1.695 → 1.639 |
| hash | 0.990 | 0.351 → 0.351 |
| many-clients | 1.000 | 4.063 → 4.095 |
| queue | 0.993 | 0.359 → 0.359 |
| sorted-set | 0.989 | 0.375 → 0.375 |

Pipeline throughput is 0.9% below baseline, compared with the earlier 3–4%
deficits, and its p99 is lower in this run. This is consistent with the reduced
allocation count, but the separate VMs do not isolate that change causally.
The 1 MiB write median is 5.6% lower, with paired ratios from 0.828 to 1.012;
1 MiB read p99 also rose. A longer targeted repeat is required before closing
that performance review. Raw data and all RSS observations are retained in
`request-admission-matched-coallocated-2026-09-07.tar.gz`; source revisions,
archive digest and paired statistics are in
`request-admission-coallocated-summary-2026-09-07.json.gz`.

The combined runtime at `8484840` passes the full Go/race/platform suites,
external adapters, differential/workload smoke, shutdown-policy matrix and
native ARM64/Intel Mac/ext4/XFS recovery. Its dedicated admission run
34180641438 passes twenty worker race repetitions, three socket-pressure
recovery repetitions and 3,649,972 parser fuzz inputs. Evidence is retained in
`request-admission-hosted-combined-2026-09-07.json.gz`. This runtime includes
the spare-buffer correction absent from the coallocation comparison above.

Review strengthened the spare-buffer fixture to compare both backing-address
identity and capacity, so a reader that ignores the refused reservation cannot
pass by growing anyway. Four interrupted-archive regression cases failed before
the fix: complete archive publication, either sample publication and source
removal. A small atomic journal now records sample offsets and removal intent;
retries verify published files and reconcile completed reports even when the
original has already been removed. Existing mismatched or unfinished partial
files are preserved and stop archiving for investigation. Ten archive tests,
including interruption around journal transitions, pass after correction.
This covers process interruption, not a power-loss durability claim. Before/after
logs and the brief buffer-identity check are in
`request-admission-review-restart-2026-09-07.json.gz`.
