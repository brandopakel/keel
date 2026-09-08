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
original has already been removed. Mismatched published archives remain untouched and report an error. Complete
staging archives can be verified and promoted; unfinished staging is retained
while bounded samples are published. Unique sample staging avoids collisions
with an earlier killed process, and a per-directory failure does not prevent
preservation of other directories. The helper still exits nonzero for incomplete
evidence. These follow-up checks are in `failed-archive-isolation-2026-09-07.json.gz`. Ten archive tests,
including interruption around journal transitions, pass after correction.
This covers process interruption, not a power-loss durability claim. Before/after
logs and the brief buffer-identity check are in
`request-admission-review-restart-2026-09-07.json.gz`.

The combined candidate `6f7ba29` passes the two-replica 128 MiB recovery job in
run 34182002957. It verifies interrupted snapshots, two history-overflow
recoveries after rewrite, two checkpoint restarts and recovery after a primary
epoch restart. Maximum observed catch-up times across the two replicas are
6.03, 4.06, 0.29 and 3.18 seconds, respectively; complete phase times including
exact value/digest verification are 6.73, 4.77, 0.96 and 3.88 seconds. These
public-runner measurements do not establish a deployment recovery SLO. Evidence:
`request-admission-larger-recovery-2026-09-07.tar.gz` and its separate summary.

The longer unprepared-memtier repeat (run 34181951552, same candidate runtime
`6f7ba29` and baseline `15ce1c0`) passes all 30 arms with no connection errors
or generator CPU warnings, but **does not pass the performance gate**. Five
30-second pairs give median throughput ratios 1.007 for 1 MiB reads, 0.830 for
1 MiB writes (individual ratios 0.834, 0.830, 1.239, 0.825, 0.731), and 0.979
for pipeline writes. Median p99s are 41.215 → 41.215 ms, 42.239 → 42.239 ms,
and 1.055 → 1.071 ms. The slower write arms have lower median request latency
but more slow responses; CPU saturation is not established by their telemetry.
Raw evidence: `request-admission-targeted-unprepared-2026-09-07.tar.gz`.

Investigation found that pinned memtier 2.5.1 passes an uninitialized `int flags`
to both SO_KEEPALIVE and TCP_NODELAY in `shard_connection::setup_socket`.
[Upstream source](https://github.com/redis/memtier_benchmark/blob/2.5.1/shard_connection.cpp#L392-L425).
This is a concrete generator defect, but does not by itself establish the
socket values or exact cause of any historical slowdown. Workflow builds now
verify the exact source checksum, initialize the option to one and verify the
patched checksum. The matched workflow retains original/prepared binaries for
a short syscall probe, requires all prepared TCP_NODELAY calls to succeed with
one, then uses that same prepared generator for both server arms. Arm reports
include the generator binary hash and preparation provenance. Previous raw
results remain unchanged and their transport setting is unverified; a corrected
comparison is required. Component allocation and independent Go arrival-generator
results do not depend on this C++ source.

The combined-candidate scheduled sweep (run 34182002957) completes 84 arms across
seven workloads, rates 1,000/50,000/150,000 per second and two alternating pairs.
No issued requests fail and none expire in the generator queue. Overload still
drops arrivals before issue; the complete per-cell drops, latency, generator CPU
and memory telemetry are in `request-admission-capacity-2026-09-07.tar.gz` and
`request-admission-capacity-summary-2026-09-07.json.gz`. These are qualified
public-runner observations, not dedicated-host capacity or deployment SLOs.

The first prepared-generator dispatches (34183835925 and 34183837950) failed
at the socket probe because the unpatched diagnostic binary had been copied in
the broad job rather than the matched job. The gate stopped both before any
paired workload began. The workflow wiring is corrected; these failed setup
attempts remain in `memtier-probe-first-attempts-2026-09-07.json.gz` and provide
no socket-option observation or performance pass.

An independent Go-generator diagnostic (run 34183839492, `e54b49e` versus
`15ce1c0`) matches the large-write mix: four connections, 32 one-MiB values,
95% writes and persistence disabled. All 24 ten-second arms complete without
issued-request failures or queue expiry. At offered rates 250 and 500/s,
neither version drops arrivals; median scheduled p99 is 2.785/2.785 ms and
3.015/2.982 ms (baseline/candidate). At 1,000/s both complete every arrival,
with p99 5.243/5.636 ms. At 2,000/s they complete 1,550.7/1,538.4 operations/s
and drop 4,493/4,616 of 20,000 scheduled arrivals; p99 is 29.884/30.409 ms.
Generator CPU is about 1.35 cores on one exposed physical core with two threads
in that overloaded cell. These results show modest costs in this different
generator/workload schedule; they neither erase nor causally explain the earlier
memtier slowdown. Evidence: `request-admission-large-write-arrival-2026-09-07.tar.gz`
and its summary with exact source, checksum, latency and generator CPU.


## Prepared-generator adoption result

Runs [34184081130](https://github.com/brandopakel/keel/actions/runs/34184081130)
and [34184079387](https://github.com/brandopakel/keel/actions/runs/34184079387)
compare `6ca7ffed5740ac2d5e8f891055b647bf336ef167` against merged baseline
`15ce1c0c696674e5c149811bc2bd071cd8554a8a`, persistence off. Both short socket
probes observe four successful TCP_NODELAY calls with zero from the original
binary and one from the prepared binary. Every measured arm uses the same
prepared generator hash recorded in its probe. This verifies these runs' socket
setting; it cannot recover the setting of historical runs.

All 84 arms pass with zero connection errors and zero generator CPU warnings.
Nine ordinary cases use three alternating ten-second pairs; the three targeted
cases use five alternating thirty-second pairs. Median paired throughput ratios
and median arm p99s are:

| Workload | Candidate / baseline | Baseline p99 ms | Candidate p99 ms |
| --- | ---: | ---: | ---: |
| 64-byte reads | 1.007 | 0.327 | 0.319 |
| Balanced 64-byte | 1.000 | 0.327 | 0.327 |
| 64-byte writes | 1.000 | 0.327 | 0.327 |
| 1 KiB reads | 1.011 | 0.375 | 0.367 |
| TTL | 0.991 | 0.343 | 0.351 |
| Many clients | 0.983 | 4.159 | 4.351 |
| Hash | 0.994 | 0.343 | 0.343 |
| Sorted set | 0.992 | 0.351 | 0.343 |
| Queue | 0.994 | 0.335 | 0.335 |
| 1 MiB reads | 0.998 | 4.383 | 4.479 |
| 1 MiB writes | 1.008 | 5.663 | 5.631 |
| Pipeline writes, 64 commands | 0.999 | 1.671 | 1.639 |

The prior 17% large-write deficit is not reproduced under the verified transport
setting; its raw evidence remains above. The targeted write ratios range from
0.976 to 1.016, and pipeline ratios from 0.993 to 1.001. These results support
adopting the scoped allocation improvement with ordinary performance near
baseline; they do not establish a general speedup, statistical confidence
intervals or dedicated-host capacity. Small-dataset RSS remains variable (for
example 64-byte write medians 13.11 to 15.12 MiB); the allocation reduction is not
a claim that RSS falls in every workload. The separate Go arrival sweep retains
its saturation drops and modest latency costs.

Complete available artifacts, socket traces, per-arm generator provenance,
latencies and RSS are in `request-admission-prepared-ordinary-2026-09-07.tar.gz`
and `request-admission-prepared-targeted-2026-09-07.tar.gz`; their separate summary
files retain archive digests and all paired ranges. No runtime source changed
after `9fb34cea`; later revisions integrate tests, harness fixes and evidence.
PRs #67/#68 were integrated after these measurements; they add soak documentation
and growth checks, with no Keel runtime change. The combined failure-diagnostic
fixture passes after including the new growth option; before/after evidence is
in `soak-merge-fixture-2026-09-07.json.gz`.
