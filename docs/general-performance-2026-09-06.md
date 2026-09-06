# General-purpose Keel performance and validation

This work optimizes Keel. GoGIF was held unchanged; its pilot is one compatibility
case, not the workload definition. No paid hosts were provisioned.

## Measured implementations

The first candidate removes the intermediate boxed RESP command array and avoids
copying nonnumeric values through integer-parser errors. A parsed sample SET
fell from 10 allocations/286 bytes to 5 allocations/166 bytes. Classifying a
nonnumeric 1 MiB value no longer allocates a copy. These are component results,
not end-to-end speedup factors.

Broader testing then exposed the cost of dense HyperLogLogs for tiny cardinalities.
The second candidate stores up to 512 nonzero registers compactly, then promotes
to the same dense registers. Hashing, counting and merge results are unchanged.
Dump/AOF bytes remain in the existing dense format, including after compact
restore. Growth and promotion are charged to the memory budget. Compact storage
does not shrink the serialized replication payload or remove the alpha frame cap.

## Executables and method

| Executable | Source |
| --- | --- |
| Prior Keel | `87cea7140ade84b64e7a9afb20ec9f26bea71d3e` |
| Parser/counter/SETEX candidate | `0485953a351abfbce46b377b60965170a952f764` |
| Compact HLL candidate | `0d283b0ec3f7e16900299e2949563db5f2ffacb6` |

All three local Keel executables were clean builds with Go 1.26.6 and `-trimpath`.
The machine was an Apple M4 Pro, 12 logical CPUs, 24 GiB, macOS 26.5.1/APFS.
Native memtier 2.5.1 and Redis 8.10.1 ran locally. Exact binaries, tool versions,
configuration, fixture hashes and raw per-run metrics are retained with the
evidence. Servers and generators shared this machine.

The original comparison has 22 workloads × three arms × three repetitions:
198 fresh-process runs with one-second warmup and five-second measurement.
Arm order rotates across repetitions. The HLL comparison has 27 runs, memory
has 81 runs, and matching `no`/`everysec`/`always` subsets have 54 runs. The current
standard harness includes two additional HLL workloads, for 24 scenarios.

The initial comparative runs enabled GC tracing on both Keel arms; CPU/heap
profiling was separate. GC tracing is now opt-in for comparisons. A separate
confirmation repeats the principal workloads with GC logging disabled. Authentication
is disabled on all timed arms and enabled in differential/operational checks.
Automatic rewrites are disabled in isolated throughput comparisons and exercised
in the separate scheduled-probe and operational tests.

These are short, closed-loop local diagnostics. Ranges are observed repetitions,
not confidence intervals. Pipelining changes latency interpretation. CPU warnings,
errors and regressions are retained. Results do not establish dedicated-host
capacity, production latency or a universal advantage over Redis.

## Results from the initial repeated comparisons

| Workload | Candidate versus its prior Keel | Other observations |
| --- | --- | --- |
| Pipeline 16, parser change | Paired throughput +7.2% median; range +6.7% to +7.5% | Median p99 0.335 → 0.287 ms |
| Pipeline 64, parser change | +7.0%; range +6.9% to +7.2% | Median p99 0.703 → 0.639 ms |
| 2,500 one-item HLLs, compact change | RSS 80.69 → 18.05 MiB | Throughput −1.5%; Redis RSS 7.77 MiB |
| Four 32-item HLL union, compact change | Paired throughput 14.11×; range 14.07–14.22× | Median p99 1.959 → 0.167 ms |
| Dense HLL reads, compact change | Roughly unchanged | Included to check the promotion tradeoff |

Most ordinary command workloads changed little. The original large-value,
large-list and nonpipelined tests do not establish a speedup. The counter case's
paired median was about 0.4% lower. There is no aggregate “Keel is faster” score.

With matching AOF policies, pipeline-16 throughput was about 6.6% higher for
`no` and 6.0% higher for `everysec`. Its `always` paired median was 4.1% lower;
short runs and storage variability limit interpretation. The implementation does
not change the append worker barrier or fsync acknowledgement contract. It does
not establish an append/rewrite-stall improvement.

The confirmation with GC logging disabled adds 36 fresh-process runs against
the original Keel and the compact candidate. Pipeline-16 paired throughput was
6.5% higher (range 5.3–6.7%) and pipeline-64 was 6.7% higher (6.2–7.5%). Their
median p99 values were 0.327 → 0.279 ms and 0.695 → 0.623 ms. Tiny-HLL RSS was
80.84 → 17.58 MiB, and the small-union paired median was 14.19×. The third HLL
repetition ran faster across multiple arms; all repetitions remain in the evidence.
The tiny-sketch throughput difference changed sign between these series, so the
stable result there is the memory reduction, not a throughput claim.

Eighteen additional ten-second diagnostics exercised a 10,000-element list,
rewrite and independent 100 Hz scheduled PINGs across the three persistence
policies. All attempts completed without recorded errors. Median scheduled-probe
p99 was 5.80 → 5.61 ms for `no`, 5.78 → 5.83 ms for `everysec`, and
6.61 → 6.83 ms for `always`. The Python generator and same-host scheduling affect
these measurements; they do not demonstrate a persistence-stall improvement.

## Memory that remains to improve

The corrected idle-connection sweep measured these median process RSS values:

| Dataset / connections | Prior Keel MiB | Compact candidate MiB | Redis MiB |
| --- | ---: | ---: | ---: |
| Empty / 1 | 11.30 | 11.38 | 7.02 |
| Empty / 256 | 11.64 | 11.64 | 11.64 |
| 2,500 × 64 B / 1 | 13.00 | 12.62 | 7.44 |
| 100,000 × 64 B / 1 | 38.20 | 36.41 | 20.38 |
| 1,000,000 × 64 B / 1 | 273.73 | 258.39 | 136.23 |
| 25,000 × 1 KiB / 1 | 47.58 | 43.23 | 41.25 |

A control connection is additional to the stated connection count. RSS includes
runtime memory and heap headroom; it is not just the keyspace estimate.

Separate live profiles measured approximately 2.67 MiB HeapAlloc when empty and
179.38 MiB with one million 64-byte values. The sampled retained heap attributes
most storage to owned key/value strings, dictionary maps and per-key objects,
with additional string boxing in SET. This identifies further per-key storage
work. It does not attribute the earlier GoGIF RSS difference to one cause.
For tiny HLLs, separate diagnostic live HeapAlloc fell from 36.69 to 3.10 MiB.
The profiler itself allocates buffers and captures force GC; those figures must
not be substituted for the unprofiled RSS measurements.

## Correctness and operations

The full Go race suite and vet pass for the runtime changes. Parser/classification
fuzzing exercised about 1.8 million inputs; compact-versus-dense HLL fuzzing added
15,141 executions. HLL tests compare exact registers/dump bytes, cache invalidation,
merges across promotion, mutable-input ownership and real heap accounting.

Local Redis differential tests checked 90,000 commands over three seeds, including
60,000 on the compact executable. Each seed includes state comparisons, rewrite
and two crash/restarts. The oracle compares the documented common RESP2 surface,
normalizes unordered collections and compares errors by class. It exposed the
noncanonical HINCRBY/counter bug; live commands now reject those spellings while
legacy AOF operations still replay. SETEX/PSETEX share SET validation and canonical
absolute-expiry persistence.

The HLL upgrade test reads old persistence, mutates/promotes/merges with the new
binary, rewrites, and recovers the new file twice with the old binary. Counts,
dump bytes and expiry remain valid. Separate replica full/delta/restart checks
pass. The original AOF is preserved as a rollback fixture.

Operational checks pass for LRU/LFU/random eviction under key and byte limits,
replica equality after eviction, two recoveries per case, and independent PINGs
while eight clients do not read large replies. The local mixed-data smoke checked
11,103 acknowledged cache writes, five checkpoints, one primary crash, four replica
crashes, stale-read rejection, fenced manual promotion and OS write-failure recovery.
Linux CI also runs all 24 workload smoke cases, Redis differential checks, HLL
upgrade/rollback, eviction/slow readers, a two-minute mixed soak and storage faults.

Longer soaks are tracked by their live `progress.json` and eventual `report.json`.
An eight-hour recovery run and a separate 48-hour continuous-primary run started
from frozen copies of the binary and harness in
`dist/validation/general/long-runs-20260906`. They are not passing evidence until their terminal reports
say so. Process crashes do not simulate power loss or prove zero asynchronous
replication loss after permanent primary failure.

## Evidence and next work

The [machine-readable evidence](general-performance-evidence.json) records all
396 comparative runs, exact source/binary identities, functional reports and
Bencher readback verification. The [compressed raw archive](../bench/results/general-2026-09-06.json.gz)
contains the native metrics and HDR histograms, complete arm reports and RSS
telemetry. Failed rehearsals and the corrected zero-key metadata run remain in
the local evidence directory and are explicitly excluded from passing comparisons.

A later Linux race job exposed a wall-clock assertion in the existing rewrite
slice test (68 ms versus a 50 ms threshold). The correction verifies each slice's
key budget, successful rewrite commit and full 200,000-key replay in both builds.
Durations remain diagnostic logs; the benchmark harness measures latency. A hard
wall-clock threshold on a shared runner is not an invariant of bounded work.
Runtime scheduling is unchanged.

Bencher retains actual local results under the exact executable source commits:
[parser report](https://bencher.dev/perf/keel/reports/01a0748c-78dd-7102-a3e6-2b6a2f337d49)
and [compact/persistence report](https://bencher.dev/perf/keel/reports/01a0748c-7c31-7ae0-8286-249401d479a7).
These are uploaded local measurements, not hosted/KVM execution. The native
export uses [Bencher's documented units and format](https://bencher.dev/docs/reference/bencher-metric-format/).
Credentials remain outside GitHub.

Remaining engineering includes string-key storage overhead, bounded large-key
serialization/rewrite finalization, command execution during ordered appends,
streamed larger snapshots and durable resumable replication. SCAN needs bounded
traversal rather than repeatedly copying the entire keyspace. Broader sorted-set
operations and open-loop/deployment-network coverage remain separate additions.
Automatic failover still needs election and fencing design. See the concrete
[persistence/replication sequence](persistence-replication-next.md) and the
[portable validation instructions](general-purpose-validation.md).

Grafana and AWS accounts are already connected. A zero-cost dedicated load/server
pair and deployment filesystems have not been established; account setup alone
does not supply them. The $0 budget remains unchanged.
