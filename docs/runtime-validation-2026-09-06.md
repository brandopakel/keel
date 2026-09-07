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
of `always`, `everysec` and `no`, preserving the acknowledged fixture. A latency
or throughput benefit from overlapping appends has not yet been established.

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
Long soaks, native architecture/temporary-filesystem CI, and dedicated deployment
results must be assessed separately before release. No AWS resources or paid
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
