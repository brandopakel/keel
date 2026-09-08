# September 7 validation closeout

This is a dated evidence snapshot. The latest published release remains alpha.3;
merged development changes below are unreleased. Source and binary identities
matter: passing older runtime soaks does not validate subsequent changes.

## Eight-hour soaks: passed

Both frozen `b14ffe093ff8a17f4b7a031ef6f66e383694af4c` recovery runs finished
at about 22:03:58 UTC on September 7. Their binary SHA-256 is
`227517de61bb5a9423ad6a62d337d727331166b89e1f7554743b88e859b6351d`.

| Run | Acknowledged writes | Primary crash recoveries | Replica crash recoveries | Checkpoints |
| --- | ---: | ---: | ---: | ---: |
| Protocol 1, worker barrier | 1,446,739 | 31 | 64 | 951 |
| Protocol 2, concurrent append | 1,444,176 | 31 | 64 | 950 |

Both pass exact cache/collection checks after recovery, quiesced manual promotion
and its restart, and the final RLIMIT_FSIZE storage-failure checks in synchronous
and worker modes. Every recorded recovery reports zero acknowledged cache values
lost under this workload's assertions. This is not a durability guarantee for
other policies or failure models.

The combined primary/replica RSS median in the first versus last hour is
106.7 to 101.7 MiB for protocol 1 and 64.0 to 60.8 MiB for protocol 2. The observed
maxima are 118.0 and 83.6 MiB. Repeated primary restarts reset process memory;
these samples do not establish continuous-uptime memory stability. The Mac also
ran development and diagnostic work, so its latency samples are not controlled
capacity measurements. Raw final reports, all checkpoints, recovery records and
watchdog logs are preserved in
`bench/results/frozen-b14ffe0-eight-hour-soaks-2026-09-07.json.gz`.

## Continuous-primary soaks: stopped, not passed

The user requested stopping the 48-hour runs before laptop sleep. Before a stop
signal was sent to the current `b14ffe0` harness, it had already exited at
22:09:40 UTC after its `ps` memory-monitoring subprocess exceeded a two-second
timeout. Its final report records 1,490,204 acknowledged writes, 962 checkpoints
and 96 replica crash recoveries. Its diagnostics could still query both servers,
but their subsequent `ps` diagnostics also timed out. The harness captured stacks
and stopped its servers. This is a failed, incomplete run; the monitoring timeout
does not establish a Keel runtime defect or its absence.

The older `b9a97e0` eight-hour siblings passed, while their continuous-primary
run had stale progress and remains unexplained. Its remaining harness and replica
were stopped with SIGINT at the user's request, and their exit was verified. The
raw terminal report retains `KeyboardInterrupt()`; a separate `user-stop.json`
preserves the prior stale progress and explains the interruption. Neither run
was restarted. Their reports, checkpoints, recovery records and diagnostic logs
are archived in `bench/results/frozen-continuous-soak-stop-2026-09-07.json.gz`.
They are separate evidence. The
merged multiplexer timeout does not establish its root cause; see
[the diagnostic assessment](soak-diagnostics-2026-09-07.md).

## Engineering completed and candidates

- PR 49 fixes term-guard restart behavior, unsafe implicit grants and mixed-version
  replication negotiation. Local guards still require external fencing before
  promotion; automatic failover is not implemented.
- PRs 48 and 50 merge sorted-set index compaction and bounded hash storage. Large
  delete-heavy collections reclaim retained indexes; matched ordinary workloads
  and the documented hash write/churn costs accompany the memory evidence.
- PR 51 releases committed AOF staging references and preflights aggregate opaque
  replication images before allocation, falling back to a snapshot when too large.
- PR 52 streams CMS/Morris rewrite encoding. PR 53 bounds GEOSEARCH workspace and
  reply construction, including sparse searches with large COUNT.
- PR 54 runs bulk rewrite file writes on one owned worker and adds I/O phase timing.
  The final synchronous handoff remains a stall source. The final matched matrix
  passes 810,000 scheduled requests and 324 rewrites; string tails improve in that
  run while sketch effects remain mixed, including a slower CMS write cell.
- PR 55 adds aggregate reservations for covered reply/workspace paths and passes
  correctness, slow-reader recovery, race and matched workload checks. It is merged. Large-string/list throughput costs and CPU-limited
  collection cells are documented; it does not cover every process allocation.
- PR 58 bounds encoded primary AOF transcripts and removes accumulated
  eviction-record arrays. Correctness, replication, torn-tail and race checks
  pass locally and in hosted race/native recovery suites. The first matched matrix
  completed off/always policies but failed baseline shutdowns for no/everysec.
  Large-write and pipeline throughput costs were observed. The corrected framing
  and admission candidate completed all 400 policy/mode arms in hosted run
  34171209014. Large SET transcript allocations fell about 49%, and the large-key
  eviction fixture fell about 80%. Synchronous pipeline throughput fell about
  3–3.5% under no/everysec sync; some p99 tails increased. This is a bounded-memory
  improvement with measured costs, not a general speedup. Review remains open;
  see [the full comparison](bounded-aof-transcripts.md).

- PR 59 introduced protocol-2 progress metrics. Merged PR 64 corrects invalid cursor
  updates, stale timestamps and epoch resets. These report received stream bytes,
  which may include an incomplete command, and do not establish applied/durable
  state, current replica availability, quorum or promotion loss.
- PR 60 makes unavailable memory-monitoring samples diagnostic errors instead of
  immediately ending the workload. It addresses the newer soak harness failure
  mode without retroactively passing that run.
- PR 61 adds a local validation wrapper with a two-minute maximum, default 512 MiB
  output budget, inherited per-file limits, reserved free space and child cleanup.
  Successful benchmark AOFs are hashed/reported and removed by default. Review
  findings have regression checks; it is merged.
- PR 62 adds an explicit shutdown grace while retaining the five-second default.
  It is merged. All six hosted large-write reproductions pass with a 30-second grace,
  including recovery of each writer's final acknowledged sequence. Original
  failures captured final fsync blocking cleanup and remain archived. This does
  not establish faster storage or explain the older stalled soak.

## Local retention

The [compact archive index](../bench/results/local-evidence-archive-2026-09-07.json)
records verified SHA-256 digests and asset IDs for the owner-accessible GitHub
**draft evidence archive**. It is not a software release. Bulk evidence stays out
of ordinary source clones. Failed/unclassified AOFs retain exact compressed
contents; successful AOFs retain hashes. Bulk latency traces retain compact
histograms, error counts, extrema and worst rows, with approximate quantiles
identified. Existing exact result summaries are preserved. Reproducible binaries,
images and download caches are removed. Uncommitted review probes and source
commits missing remote branch refs were archived before removing 35 old worktrees.

This batch removed 10.66 GiB of archived evidence and another 1.25 GiB of generated
files/upload copies, in addition to obsolete checkout contents. Small terminal
reports keep both local soak-status commands usable. Five orphaned September 3
test listeners were also stopped after checking their temporary test directory,
PPID 1 and absence of connected clients. No local soak is restarted. Full/race,
large-dataset and operational testing now belongs on hosted runners within $0.
The local figures exclude unrelated applications and their active build output.

## Work still open

A complete temporary-allocation contract must cover parsing, other mutations,
replication transport/history, rewrite images and borrowed retired values, and
compaction overlap. PRs 55/58 are scoped improvements, not completion of that
contract. Final rewrite sync/rename/directory-sync work requires a defined ordered
handoff before commands can continue through it. Large commands still have CPU
and filesystem stalls; the transcript draft may trade memory for more write calls.

The older stalled soak and historical Intel pending-reply/Apple Silicon fairness
observations remain unexplained despite successful repeats. A separate large-file
startup observation is now timed explicitly and has matched successful repeats;
that is not evidence of a runtime fix for those earlier observations.

Dedicated deployment testing needs an existing suitable host within the $0
budget. More real application traces require selected applications and workload
access; GoGIF remains one unchanged pilot. Public Linux/macOS CI, ext4/xfs
fault/recovery tests and synthetic workload matrices provide useful evidence but
do not substitute for dedicated deployment or actual application traces.

## Next engineering gates

| Priority | Remaining work | Evidence required to close it |
| --- | --- | --- |
| 1 | Finish transcript admission and shutdown integration | Reviewed source; all policy/mode pairs complete with matched settings; no unexplained correctness failures; any throughput/memory tradeoff stated explicitly. |
| 2 | Complete aggregate temporary-allocation coverage | Bound parsing, mutation, replication, rewrite and compaction overlap before allocation; demonstrate rejection/recovery under mixed slow clients, large requests and persistence pressure. |
| 3 | Reduce remaining large-command and rewrite finalization stalls | Define ordered publication across final sync/rename/directory sync; fault injection at each phase; paired tail-latency measurements with unchanged durability. |
| 4 | Extend replication recovery and churn coverage | Larger snapshots, sustained updates, outages and multiple lagging replicas; verify final state and acknowledged offsets, bounded memory, recovery time and failure retention. |
| 5 | Establish broadly applicable capacity limits | Load sweeps, expiry storms, tenant mixtures, large collections and client reconnect/slow-reader mixes with generator headroom; compare repeated same-host baseline/candidate/Redis runs. |
| 6 | Resolve deployment and application evidence gaps | Existing suitable dedicated hosts and representative application traces within $0; retain GoGIF as one unchanged pilot. |

Public-runner results cannot establish dedicated-host capacity. Short operational
runs on ext4/xfs and native Linux ARM64/Intel Mac checks remain useful; extended
validation runs on hosted runners and does not keep the laptop awake. Automatic
failover requires enforceable external fencing or a separate election design;
embedding, partitioning and transactions remain separate architectural commitments.

## Follow-up storage and validation work

PRs 61, 62, 63, 64 and 65 are merged. PR 65 handles temporary directories disappearing
during resource scans and prunes separate Go intermediate-build directories on
both success and failure. It preserves ordinary failure evidence. The final
transcript policy comparison is [run 34171209014](https://github.com/brandopakel/keel/actions/runs/34171209014);
all 400 arms passed, with compact reports archived in the repository and
563.65 GiB of cumulative disposable AOF output pruned on the runners. None of
that raw output was downloaded to the laptop. PR 58 remains open for review;
the measured throughput and tail-latency costs remain visible in its report.
Its earlier corrected off/always comparison completed 160 arms. CodeRabbit's
first transcript review was skipped at the included-review limit; a green status
for that skip does not establish a completed review. Paid reviews are not enabled.

Forty old/merged/control worktrees have been removed. A further 561 MiB
of temporary environments, binaries and early release evidence was pruned after
archiving the evidence. The interrupted Go test's 141 MiB of intermediate builds
was also pruned; its small test fixture remains archived. The archive index now
identifies 16 verified draft-release assets. Local soaks remain stopped.


## Current hosted follow-up

PR #58 merged as `15ce1c0` after the final `0c74e92` review and required checks
passed. Review reproduced a panic and incorrect replication publication after
an AOF short write; failed drains now stop publication. The 400-arm persistence
matrix and two-replica 128 MiB recovery test passed on their recorded runtimes.
The original Intel three-second rewrite wait remains unexplained; ten matched
repeats passed the original gate. The current fixture logs three seconds as a
diagnostic threshold and has separate deadlock, process, arm and job watchdogs.
Details and performance costs remain in [the transcript report](bounded-aof-transcripts.md).

The [capacity sweep](https://github.com/brandopakel/keel/actions/runs/34174358161)
completed 140 arms with no issued-request failures and generator queue drops
under overload. The [paired profiles](https://github.com/brandopakel/keel/actions/runs/34176103471)
completed 36 diagnostic arms. Their source revisions, measurements and
limitations are in [the combined report](transcript-profile-capacity-2026-09-07.md).
These do not establish a general speedup or dedicated-host capacity.

PR #66 adds shared request allocation admission before TCP buffer growth and
RESP decoding, plus bounded command-name conversion and diagnostics. Its
[report](request-allocation-admission.md) records reproduced allocation gaps,
initial hosted parser diagnostics and pending final validation. Remaining
coverage includes unmodeled mutation/persistence/replication/rewrite allocations,
compaction overlap and kernel memory. The global allocation program is not complete.

[Four-hour ext4/XFS recovery jobs](https://github.com/brandopakel/keel/actions/runs/34174359818)
remain running at this update on earlier runtime `6966b55`. They do not count as
passes or validate later fixes. No local soak is running; new local checks are
brief, guarded and pruned after archiving compact evidence on GitHub. Alpha.3
remains the latest published software release.
