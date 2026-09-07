# Engineering status — September 7, 2026

This is the current rollup for the broad Keel engineering program. Historical
reports retain their original binaries and observations; their old “pending”
statements are superseded here. The latest published release remains
`v0.1.0-alpha.3` (September 5). No release has been made from this closeout work.
GoGIF remains an unchanged pilot, and spending remains capped at zero.

## Four initial findings: complete

PR #27 merged all four corrections with Go/race, Docker, differential, native
architecture/filesystem and matched traversal validation:

1. The failover design requires an external authority to verify isolation of the
   old process/incarnation before activating another writer. A local term is not
   fencing. Finite models and counterexamples exercise the design; a provider,
   election system and automatic promotion are not implemented. PR #31 merged
   local term guards on September 7. The subsequent [restart correction](term-guard-recovery.md)
   prevents a restarted process from claiming an observed successor's term and
   restores term-zero rolling compatibility. These guards do not provide safe
   failover by themselves. See [the original review](failover-31-review.md).
2. Go 1.26 is the source/CI floor, current stable Go is tested, and Docker pins
   Go 1.27.1. The README support statement now matches the supported 1.26/1.27 lines.
3. SCAN handles empty MATCH, empty TYPE and case-insensitive TYPE with regression
   and Redis differential coverage.
4. Stable 64-slot pages replace whole-shard traversal. SCAN charges bounded work,
   pattern matching has its own budget, and rewrite/protocol-2 snapshots enumerate
   key names incrementally. Broad and longer targeted matched runs satisfy the
   documented adoption gate. These are cooperative bounds, not a real-time SLA.

Evidence: [closeout](engineering-closeout.md), [traversal](keyspace-traversal.md).

## Subsequent implementation and evidence

| Area | Merged and validated | Remaining limits |
| --- | --- | --- |
| Storage and appends | PR #20: typed strings, ordered concurrent appends, collection rewrites, restart compaction, protocol 2 and sorted-set commands | Command execution remains serial; no universal throughput gain |
| Traversal/rewrite resources | PR #28: immutable string/member fragments, 64 KiB stream slices, dirty-name count/byte/duration budgets | Opaque image construction and filesystem writes/finalization still stall |
| Client fairness | PR #37: bounded pipeline turns, output-drain scheduling and safe command resumption | Individual commands remain atomic; no hard latency SLA |
| Rewrite sync | PR #43: one background snapshot preflush, explicit completion notifications and safe waker detachment | Final dirty-tail sync, writes, rename and directory sync remain synchronous |
| Opaque rewrites | PR #44: retain one binary image and emit 64 KiB fragments | Constructing that immutable image still requires full serialization |
| Churn memory | PR #29/#36/#38/#45: release large empty TTL tables and incrementally rebuild sparsely occupied TTL, lookup and set membership maps | Partially occupied pages and hash/sorted-set maps still retain capacity; no RSS guarantee |
| Allocation admission | PR #32/#35/#42: preflight amplified replies and destructive canonical records; encode accepted dumps once | Aggregate transient reservations and some opaque persistence construction remain incomplete |
| Linux throughput | PR #30: avoid unchanged readiness registrations on Linux | Darwin optimization was deferred after unresolved Intel test failures |
| Client compatibility | PR #39: go-redis, Redigo, redis-py, node-redis and ioredis; 35 invocations across persistence modes and two restarts | Tested RESP2 subset only; no RESP3, transactions, cluster or every library API claim |
| Operations and capacity | PR #34: scheduled overload sweeps, expiry storms, tenant mixtures, large collections, append diagnostics, two-replica 128 MiB recovery | Free public runners are not dedicated hosts or deployment SLO evidence |
| Soak reporting | PR #40: stale-progress detection and a stack-producing watchdog for new soaks | The old 48-hour harness stalled; its cause is unresolved |
| Heap validation | PR #41: larger sparse-HLL measurement population, unchanged tolerance | Process-wide heap deltas remain measurements with noise |

Rewrite preflush (PR #43), fairness, opaque-record copy reduction and set
compaction have merged. PR #31 subsequently changed the serving runtime. The
frozen b14ffe0 soaks and native archives therefore do not qualify the term guards
or their restart corrections; those changes require their own validation.

## What the measurements establish

- Typed string storage reduced RSS by 16.5% in the recorded million-key experiment.
  TTL and lookup compaction separately recover about 3.44 MiB and 2.33 MiB in
  100,000-to-1,000-key heap fixtures. These are distinct workloads and metrics;
  they do not establish lower memory than Redis for every dataset.
- Linux readiness caching improves the tested small-read workload by a paired
  median 13.7%; large-value and other targeted results are near baseline or noisy.
- The matched append study shows 22–36% throughput gains in its no/everysec cases.
  `always` is mixed, including regressions. Earlier results showing no gain remain
  valid observations of their earlier candidates and harnesses.
- DUMP admission reduces a refused 65 MiB list dump from 629 MB of allocation to
  32 bytes, and an accepted 4 MiB dump from 28.8 MB to about 4.2 MB. KEL1 payloads
  and checksums remain compatible with the retained legacy fixtures.
- Fairness raises the observer tenant from roughly 163–170 to 1,000 requests/s
  under the tested competing pipelines, reducing scheduled p99 from 145–197 ms
  to 1.3–6.5 ms. Ordinary pipelined throughput costs about 1–2% in the final
  matched adoption run; that tradeoff is explicit.
- Opaque rewrite startup for a 4 MiB count-min sketch falls from about 12.6 MB
  to 4.27 MB allocated, and first-cycle output is limited to 64 KiB. Serialization
  CPU remains synchronous.
- Rewrite preflush passes 36 matched arms with 108 rewrites, 1,080,000 completed
  requests and no dropped or failed requests. Scheduled p99.9 falls from about
  9.4–11.0 ms to 3.9–4.9 ms across the six tested persistence/write mixtures.
  Ordinary qualified workloads remain near baseline; the generator-limited final
  many-connection row is excluded from capacity evidence.
- Set membership compaction recovers about 3.44 MB of heap in the
  100,000-to-1,000-member fixture. Matched ordinary workloads remain near baseline;
  saturated large-set and sorted-set sweeps retain their drops and do not establish
  a throughput gain.
- Scheduled capacity includes 140 corrected initial arms and 32 collection-parser
  follow-ups. Above saturation, drops remain in the results. Removing generator
  allocation overhead improved the harness; that is not a Keel speedup.
- Two replicas pass interrupted 128 MiB snapshots, 32 MiB history overflow,
  checkpoint restart, primary epoch reset and exact acknowledged-counter checks.
  Recovery time depends on the tested host, dataset and offered load.

Each statement has its exact source, settings, raw attempts and limitations in
[TTL compaction](incremental-ttl-compaction.md), [lookup compaction](incremental-lookup-compaction.md),
[readiness](readiness-registration.md), [append diagnostics](append-cost-validation.md),
[DUMP admission](dump-allocation-admission.md), [capacity](capacity-validation.md),
[fairness](client-fairness.md), [opaque records](opaque-rewrite-records.md),
[client compatibility](client-library-compatibility.md),
[rewrite sync](rewrite-preflush.md), and
[set compaction](set-index-compaction.md).

## Long-run and release gates

Both eight-hour frozen recovery runs have terminal PASS reports. Protocol 1
recorded 1,585,893 acknowledged writes; protocol 2 with concurrent appends recorded
1,574,932. Each completed 31 primary and 64 replica crash recoveries, promotion
checks and storage-fault checks. They validate only source `b9a97e0` and binary
SHA-256 `aeed3178e90e12664f8f715a7adb2889c6bffcdf36fbba076cbe9d64862c8b31`.

The 48-hour continuous-primary run stopped reporting progress at 10:29:35 UTC
after 739 checkpoints and 1,091,864 acknowledged writes. Its processes remain
alive. A native sample shows the Python harness sleeping but does not identify
the Python call site or establish a Keel fault. It is stalled, not a completed
soak. The frozen processes were preserved; new harnesses have a progress watchdog.
See [soak evidence](soak-progress-observability.md).

A combined candidate passes all nine alpha.2 upgrade/rewrite/restart/backup
rollback cases across no/everysec/always and sync/barrier/concurrent append modes.
Its guarded ten-minute recovery run records 75,221 acknowledged writes, three
primary and six replica crash recoveries, and promotion checks. These results
predate the final rewrite wakeup fixes and do not qualify the final revision.

Combined archive validation run 34128192062 failed on Linux in the latency
diagnostic's insertion sort. The rewrite loop had already finished. The diagnostic
now separates worker waits and uses O(n log n) sorting; PR #43 also fixes a
reproduced idle wakeup loop while waiting for original-log sync. Combined archive
validation 34130106439 passes Linux AMD64/ARM64 and macOS
Intel/ARM64 native execution, archive/checksum/installation checks and all nine
alpha.2 upgrade cases on each platform (36 total). The exact source is
`b14ffe093ff8a17f4b7a031ef6f66e383694af4c`; no tag or publication was performed.
An Intel pending-reply timeout in
PR #45 remains unexplained after thirty focused repetitions per arm and three
full suites per arm pass on a matched host. Both failed attempts are preserved.

Fresh frozen guarded runs began at 14:03 UTC on that combined source: eight-hour
protocol-1 recovery, eight-hour concurrent protocol-2 recovery, and 48-hour
continuous-primary protocol-2 recovery. Each is running, not passed. Binary
SHA-256 is `227517de61bb5a9423ad6a62d337d727331166b89e1f7554743b88e859b6351d`.
The local command `~/.local/bin/keel-current-soak-status` verifies progress and
process identity. The Mac must remain running. The initial frozen-bundle launch
failed before workloads because a Python dependency was omitted; the corrected
bundle includes it and passed import preflight. Both launch attempts and all
native reports are retained in
`bench/results/combined-native-validation-2026-09-07.json.gz`.

Later PR #43 CI recorded a Go 1.26 Apple Silicon fairness-backpressure observation
failure and an Intel HTTP 504 while downloading alpha.3. The download failure is
external; the fairness observation remains unexplained after the same-runner diagnostic
34131652665 passed 600 focused subcases and six complete suites. The regression
now records the missing counter and client state if it recurs. These runs do not erase the
passed archive evidence, nor does archive success resolve those failures.

Final combined archive validation 34134709336 also passes all four native
platforms and all 36 alpha.2 upgrade/rewrite/restart/backup rollback cases. Each
concurrent case now exercises four simultaneous authenticated writers and verifies
their acknowledgments and recovered values. Source
`6fb1ed6308d997b5f23ad08ac5c74af8b005613e` has the same serving code as the frozen
`b14ffe0` candidate. The archives pass checksum and installation checks, and the
workflow's publication job is skipped. Full native reports are retained in
`bench/results/combined-native-writers-2026-09-07.json.gz`.

Before another release: finish the guarded long workloads and ensure all release
candidate reviews are closed, then build the final tag and verify its downloaded
archives, checksums and installation. Repeat candidate validation if serving code
changes. Native development-archive checks do not validate a future tagged release.

## Work still requiring engineering or deployment evidence

1. Finish guarded long soaks. Expanded native upgrade validation and the rewrite
   review/adoption gate are complete: final ordinary matched evidence is recorded
   alongside 36 rewrite arms and 1,080,000 completed requests. Historical Mac
   observations remain unexplained after matched repeats; investigate any recurrence
   before making stronger release or availability claims.
2. Bound aggregate transient allocations before construction, reduce remaining
   opaque serialization pauses, and address final filesystem handoff stalls.
3. Implement and validate hash/sorted-set map compaction and address partially
   occupied pages. The new [retention profile](collection-retention-profile.md)
   isolates about 5.17 MB and 3.44 MB of reclaimable map capacity after a
   100,000-to-1,000-entry shrink, rising to about 83.68 MB and 55.76 MB after a
   million-entry high-water population. These are measured targets, not shipped savings;
   small-collection overhead and bounded traversal still need a design.
4. Extend sustained overload, expiry/eviction/rewrite mixtures and multiple lagging
   replicas on larger deployments. Interrupted in-memory snapshots resume over a
   connection; a process crash during a snapshot still requires a new snapshot.
5. Collect additional real application traces and library feature demands. Add
   compatibility in response to those workloads; broad command parity is a separate
   commitment. GoGIF alone is insufficient coverage.
6. Run dedicated-host/network/storage experiments when suitable infrastructure is
   available within the zero-dollar budget. Existing account/adapters do not imply
   that paid KVM/AWS resources or deployment tests have run.

Embedding, partitioning, transactions and automatic failover remain separate
architecture commitments. The program is materially further along, but neither
the remaining long-run evidence nor those architectural features are complete.

## Validation refinements

The concurrent upgrade cases now launch four authenticated writers after loading
the alpha.2 file. They verify all 400 increment replies exactly once, 400 SET
acknowledgments, and all resulting values after rewrite and restart. All nine
local cases pass. The earlier four-platform archive reports exercised each
configuration with sequential traffic; final native run 34134709336 passes the
expanded concurrent-traffic cases on all four platforms. Neither test establishes
physical append overlap or a durability guarantee beyond its configured policy.

Retention diagnostics now assert the grown and surviving collection sizes before
sampling and verify each surviving value/score after the rebuild probe. All 24
measurements pass those guards. The later ARM CI failure in 34133441575 was an
alpha.3 download HTTP 504 after retries; it was not a runtime test failure.
Raw evidence is in `bench/results/upgrade-writers-and-profile-guards-2026-09-07.json.gz`.
