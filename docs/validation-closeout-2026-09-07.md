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
heartbeat proposal does not establish its root cause; see
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
  correctness, slow-reader recovery, race and matched workload checks. It remains
  open at this snapshot. Large-string/list throughput costs and CPU-limited
  collection cells are documented; it does not cover every process allocation.
- Draft PR 58 bounds encoded primary AOF transcripts and removes accumulated
  eviction-record arrays. Correctness, replication, torn-tail and race checks
  pass locally. Its controlled policy/mode matrix and review remain pending;
  local large-write probes showed a possible throughput cost, so adoption is held.

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
