# Runtime follow-up evidence

This follows the parser/HLL milestone merged in PR #19. The follow-up is an
unreleased candidate. GoGIF remains unchanged; the fixtures cover general cache,
collection, connection-count, persistence and recovery workloads.

## String memory

Typed string objects remove an interface box and redundant type metadata,
reducing allocation by 24 bytes per stored string. Collections retain their
existing accounting. A three-repetition local matrix compared `17517f6` against
the typed-string-only `f2bd549` binary over nine dataset/connection cases, for
54 arms. These source revisions precede rebasing onto the squash-merged PR.

| Dataset | Baseline RSS MiB | Typed strings RSS MiB |
| --- | ---: | ---: |
| Empty, 1 connection | 11.47 | 11.58 |
| Empty, 256 connections | 11.88 | 11.75 |
| 2,500 × 64 B, 1 connection | 12.70 | 12.56 |
| 2,500 × 64 B, 256 connections | 13.14 | 12.73 |
| 25,000 × 64 B | 18.72 | 18.41 |
| 100,000 × 64 B | 36.12 | 33.34 |
| 1,000,000 × 64 B | 256.45 | 214.19 |
| 2,500 × 1 KiB | 15.78 | 15.62 |
| 25,000 × 1 KiB | 43.52 | 42.47 |

The million-key case reduced median RSS by 16.5%. RSS includes allocator/GC
effects and is not a direct per-key heap measurement. This did not include a new
Redis arm; it does not establish parity with Redis's previously measured string
memory. The raw archive preserves binary hashes, generator build, all reports
and summaries: [typed-string measurements](../bench/results/typed-strings-2026-09-06.json.gz).

That archive also retains 144 throughput arms: all 24 general workloads, baseline
and typed-string candidate, three repetitions each. These overlapped the old
long soaks and large replica replays; results vary substantially between arms.
They are retained as confounded measurements, not evidence of a universal
throughput improvement or regression. Dedicated-host comparisons remain needed.

## Failed long soaks and recovery

Both frozen `0d283b0` long runs failed with the original ten-second readiness
timeout. The 48-hour continuous-primary run had acknowledged 698,540 cache writes
and completed 28 replica crash recoveries. The eight-hour recovery run had
acknowledged 799,623 cache writes, with ten primary and 21 replica recoveries.
Neither completed its requested duration. Original logs, AOFs and terminal
reports remain preserved locally; later diagnostic passes do not replace them.

Restart reset the automatic rewrite growth baseline to the entire AOF size.
Repeated replica restarts therefore continually raised the compaction threshold.
The candidate estimates a conservative live-state baseline until the next actual
rewrite. A deterministic regression test repeatedly overwrites, reopens,
compacts and replays the log.

Recovery diagnostics operated on APFS clones, with an explicitly separate
60-second deadline:

| Failed run | Replica input bytes | Candidate compacted bytes | Older-binary restart |
| --- | ---: | ---: | ---: |
| Continuous primary | 1,590,573,103 | 1,888,028 | 30.2 ms |
| Recovery | 1,800,536,948 | 1,888,030 | 28.6 ms |

Both diagnostics recovered the primary's latest 1,000 acknowledged cache values
without loss. Candidate replica state matched the original replica's recovered
state, and an older binary reopened the compacted AOF identically. Original
replicas had lagged behind their primaries: two cache keys differed in each run,
plus three collection keys in the recovery run. This is asynchronous lag, not
evidence of zero replication loss. Replay in the quieter diagnostics took about
2.7–3.9 seconds, so the files alone do not reproduce every original timeout.

Raw diagnostic reports: [continuous-primary recovery](../bench/results/failed-soak-recovery-2026-09-06.json),
[overnight recovery](../bench/results/overnight-failure-recovery-2026-09-06.json).

## Candidate behavior and validation boundaries

`-aof-concurrent-append` remains off by default. It permits conservatively bounded
string command runs while an immutable append is pending, and gates their replies
and dependent reads by the required prefix. Unsupported runs, eviction pressure
and queue pressure use a drained barrier. This is not general concurrent command
execution or preflight admission for every command. Controlled writer tests cover
short writes, sync failures, dependent reads, client disconnects and rewrite fences.
TCP tests exercised 32 clients, 24,000 commands and two crash restarts under each
of `always`, `everysec` and `no`, preserving the acknowledged fixture. The local and hosted comparisons below show mixed results; they do not establish
a general latency or throughput benefit from overlapping appends.

Large hash/list/set/sorted-set rewrites now yield between bounded slices and
reconcile mutations with canonical deletion/replacement. Single oversized members,
opaque images, initial key enumeration and final syncs remain stall sources.
The hash cursor's allocation/CPU tradeoff is documented with raw measurements in
[the protocol 2 guide](replication-v2.md).

The candidate also adds `ZCOUNT`, `ZRANGEBYSCORE`, `ZREVRANGEBYSCORE`, `ZINCRBY`,
`ZPOPMIN` and `ZPOPMAX`. Canonical `ZADD`/`ZREM` persistence keeps these operations
readable by older binaries. Two 30,000-step Redis differential runs passed with
concurrent appends under `always` and `everysec`; this does not imply complete
Redis compatibility. `SCAN`, transactions, embedding and partitioning remain open.

[Protocol 2](replication-v2.md) adds larger streamed snapshots, compact operation
updates and validated restart checkpoints. Its unit and separate-process tests
cover normal recovery and injected faults. It remains opt-in and asynchronous.
Long soaks and dedicated deployment results must be assessed separately before
release; completed native architecture and temporary-filesystem checks are below. No AWS resources or paid
benchmark hosts have been provisioned for this follow-up.

## Completed follow-up checks

All twelve build/test jobs passed at PR head `fceca50`, including Go 1.22/stable,
the race detector, the general Redis/workload suite, native Linux ARM64 and Intel
Mac, and ext4/XFS temporary-filesystem recovery. Both published alpha.3 archives
executed on their native architectures and passed upgrade/backup-rollback checks.
The [native and filesystem reports](../bench/results/runtime-native-filesystem-2026-09-06.json)
retain binary hashes, platforms and per-check outcomes from
[the workflow](https://github.com/brandopakel/keel/actions/runs/34080321993).

The frozen runtime source `0859160` passed the 32 MiB replication integration
test: interrupted snapshot transfer, three large-collection mutations using
401 downstream bytes, and a checkpoint restart using 281 downstream bytes.
Primary restart, changed AOF and history-overrun full synchronization also passed.
See [the local report](../bench/results/replication-v2-local-2026-09-06.json).

A preceding protocol 2 build with packed history, before the hash cursor change,
completed a [15-minute recovery soak](../bench/results/replication-v2-soak-15m-2026-09-06.json):
105,471 acknowledged cache writes, four primary crash recoveries, ten replica
crash recoveries, fenced promotion and both OS file-size-limit fault cases. The
worker fault case enabled concurrent appends. Its distinct binary hash remains
in the report; it is not a long-duration test of the final candidate.

## Matched runtime matrices

The frozen runtime source `0859160` was compared with the PR #19 baseline, then
with itself under worker and concurrent append modes. All **252 arms** passed:
24 no-AOF cases × two binaries × three repetitions, plus six cases × two modes ×
three repetitions under each of `no`, `everysec` and `always`. Every pair used
fresh servers, matching fixture hashes, the same native memtier 2.5.1 generator,
three measured seconds and one warmup second. The arm order was rotated.

The no-AOF cases include different value sizes, connection counts, pipeline
depths, hit/miss/hot-key patterns, expiry, working-set size, hashes, sets, sorted
sets, queues, counters, HLL and reconnects. Median paired throughput ratios
(candidate/baseline) ranged from **0.983 to 1.023** across these 24 cases.

Local timings are confounded by shared-host activity: an unrelated compiler was
observed using 358% CPU and WindowServer about 99% during the matrix. These runs
did not intentionally overlap our soaks, but are not isolated-host measurements.
Some individual ratios varied far more than the three-run medians.

For the append-mode comparison, both arms used the identical candidate binary
and worker appends; only the concurrent flag changed. Values below are medians
of paired throughput ratios, concurrent/worker; higher is faster.

| Case | `no` | `everysec` | `always` |
| --- | ---: | ---: | ---: |
| Balanced 64 B | 1.093 | 1.383 | 1.664 |
| Write-heavy 64 B | 1.108 | 1.005 | 1.083 |
| Read-heavy 16 KiB | 0.897 | 0.906 | 1.924 |
| Many clients | 0.980 | 1.035 | 2.198 |
| Pipeline 16 | 0.878 | 0.877 | 1.236 |
| Pipeline 64 | 0.814 | 0.980 | 1.083 |

For `no`/pipeline-64, all three ratios were about 0.81 and median p99 increased
from 0.679 to 0.775 ms. For `always`/many-clients, median p99 fell from 17.791 to
10.047 ms. The `always` pipeline cases gained throughput while median p99 rose
slightly. The results support keeping concurrent append opt-in and investigating
the pipeline cost; they do not establish portable speedups.

Separate ten-second CPU profiles for `no`/pipeline-64 were dominated by Darwin
syscall and scheduler frames, with limited attribution to command execution.
They did not isolate the cause of the regression. No causal claim or additional
optimization is based on those profiles. The [raw matrix and profile archive](../bench/results/runtime-local-matrices-2026-09-06.json.gz)
contains 540 files with source manifests, report hashes, memtier JSON, independent
profile data and the host-noise observation.

## Free hosted Bencher batch

All **15 jobs / 60 arms** completed on Bencher's `intel-v1` Firecracker runners,
with **1,884,510 attempts, zero errors and zero dropped probes**. The raw CSV
samples were independently recomputed and matched all **840 published BMF
measurements**, including scheduled probe latency percentiles. Binary hashes,
clean source identities, arm order, loopback setup, telemetry and GC traces were
also checked. No paid resources or additional review credits were enabled.

Each job ran the PR #19 baseline synchronously, then three modes of the candidate:
synchronous, worker, and concurrent worker. The four-arm order rotated over five
repetitions for each policy (`off`, `everysec`, `always`). All sources were rebuilt
with Go 1.27.1 and CGO disabled. The candidate source was `6e581cd`; later review
fixes change tests, validation and a code comment, not its runtime behavior.

| Policy | Candidate sync / baseline throughput | Concurrent / worker throughput | Concurrent / worker probe p99 |
| --- | ---: | ---: | ---: |
| Off | 0.998 | 1.004 | 0.967 |
| Every second | 1.003 | 0.997 | 0.925 |
| Always | 1.003 | 0.996 | 1.009 |

These are medians of paired ratios. Lower probe-p99 ratios are better. The
individual concurrent/worker p99 ratios ranged from 0.705 to 1.268 for `everysec`
and 0.660 to 1.284 for `always`; the batch does not establish a reliable tail
improvement. The worker arm itself ran about 3% below synchronous throughput
under both persistence policies. When AOF is off, worker/concurrent flags are
disabled and their labels represent repeat controls.

This hosted fixture uses one closed-loop mixed cache/list writer, a 100 Hz
scheduled PING probe, a 10,000-element list and a rewrite with AOF enabled.
Five-second arms with the Python client and server sharing a four-vCPU guest
are not sustained capacity tests, a broad workload matrix, or a reserved pair
of client/server hosts. This fixture establishes **no concurrent-append speedup**.
The broader local matrix and this narrower hosted fixture answer different
questions; neither supports a universal performance claim.

[Verified results and raw evidence hashes](../bench/results/runtime-bencher-hosted-2026-09-06.json)
retain all job/report/runner IDs. Raw archives remain locally retained and can be
retrieved through the authenticated project Jobs API. The [verification script](../bench/results/verify-runtime-bencher.py)
recomputes this batch from those archives. Published metrics use the separate
`bencher-intel-v1-runtime-four-arm-gc` testbed in the
[Keel project](https://bencher.dev/perf/keel).

## Review fixes and replacement long runs

All six initial CodeRabbit findings were fixed in `b9a97e0` and their threads
resolved. Downloaded native release archives now require repository-pinned
SHA-256 digests before extraction or execution. Test clients are isolated,
owner-thread assertions replace goroutine polling, each connection's counter
results must increase, and a missing AOF cannot hide a startup timeout.
Server race tests and all three 24,000-command TCP ordering/crash tests passed
again. All twelve CI build/test jobs passed at `b9a97e0`, including the pinned
native archive checks and ext4/XFS recovery.

The follow-up review found that Python optimization could remove the checksum
assertions. Archive validation now uses explicit failures, with a native CI
regression test that rejects a mismatched independent pin or checksum sidecar
before extraction under both ordinary Python and `python -O`.

At **2026-09-07 04:17 UTC**, three replacement soaks started on a separately
frozen, clean `b9a97e0` build and copied harness:

- Eight-hour protocol 1 recovery, worker appends, primary crash every third cycle.
- Eight-hour protocol 2 recovery with concurrent appends and the same crash cadence.
- Forty-eight-hour protocol 2 with concurrent appends, a continuous primary and
  repeated replica crash/recovery cycles.

All use five-minute crash cycles and keep the original ten-second startup
readiness deadline. They share the local Mac/APFS host; their latency is not a
controlled performance comparison. **They are running, not passed.** The
[launch snapshot](../bench/results/runtime-long-soaks-launched-2026-09-06.json)
records binary/harness hashes and configuration. Terminal reports, process start
identities and progress must be inspected for actual completion. The original
failed runs remain preserved separately.

The Benchmark workflow also has an explicit `soak-runtime-linux` entry point for
four-hour protocol 2/concurrent recovery runs on separate ext4 and XFS loopback
filesystems. Each uses an owned 512 MiB image, five-minute crash cycles and a
270-minute job timeout; normal PR checks retain their three-minute workload and
15-minute timeout. Standard public-repository Linux runners keep this within the
$0 budget under [GitHub's billing rules](https://docs.github.com/en/billing/concepts/product-billing/github-actions).
These are longer temporary-filesystem tests, not reserved deployment hosts or
power-loss tests. A submitted or running job is not a completed soak.

Before release: complete and assess the replacement long runs, review any new
findings, and validate the final release candidate. Dedicated deployment hosts,
real power-loss behavior, larger opaque values, complete command admission,
SCAN and automatic failover remain outside the evidence established here.
