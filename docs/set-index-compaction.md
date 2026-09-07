# Set membership map compaction — September 7, 2026

Shrinking the dense member slice did not shrink a set’s membership map. A set
that grew to 100,000 members and retained 1,000 still held about 3.4 MiB in that
sparse map. The candidate begins a shadow-map rebuild when a sufficiently large
member slice reaches its existing shrink threshold. Maintenance copies bounded
positions from the dense slice; insertions, last-member moves, deletions and
random shuffles mirror their positions into the shadow map. The old map stays
authoritative until completion. Substantial regrowth cancels the rebuild.

The compactor adds one pointer to Set (40 to 48 bytes; both occupy Go’s 48-byte
allocation class on tested 64-bit builds), with no per-member metadata. Large
shrink events allocate the replacement state lazily. Collection maintenance,
TTL compaction and key lookup compaction rotate through one shared 1,024-work
budget and cooperative 1 ms target. Visiting a key/empty page and copying a
member each consume work. Logical membership and order do not change, so no
persistence record is emitted. Existing member-slice shrink copies remain
synchronous; this does not bound every collection command’s latency.

The 100,000-to-1,000 fixture retains its map on baseline (32 bytes of noise),
while the candidate recovers 3,440,296 bytes in the race run. Mutation/shuffle,
regrowth, multiple-set budget and persistence/replay regressions pass. Full
local tests/vet pass. Raw baseline failures and candidate evidence are in
`bench/results/set-index-compaction-2026-09-07.json.gz`. Hosted compatibility and throughput results follow. The compactor does not
change the tested small-set allocation class; no process RSS improvement is claimed. Hash/ sorted-set maps
and partially occupied key pages are separate remaining memory work.

Review closeout strengthens the serving-maintenance test: the fixture must
start with a pending rebuild, and the scheduled hook must finish it. This fails
if the collection hook is removed. The live member sequence is checked exactly
before/after maintenance. Replay deliberately checks unordered membership;
set iteration order across restarts is not a client contract. A further large
shrink restarts the shadow map to avoid retaining its own high-water capacity.

Matched run 34127226459 completes three ten-second repetitions at 790844e
against 66f8ceb3. Small-read, TTL, many-client and ordinary-set throughput ratios
are 0.998, 0.993, 1.005 and 1.001, respectively; ordinary-set p99 is 0.343 ms
in both arms. Large-set reads are 0.998 with equal p99, but all six large-set
arms hit generator CPU warnings. That case cannot establish server capacity.
Raw matched evidence is in `bench/results/set-compaction-matched-2026-09-07.json.gz`.

The scheduled native generator now also supports set and sorted-set whole-key
reads, retaining its nonallocating reply discard and bounded admission queue.
Preload verifies all 4,096 members; distinct set/zset members add a numeric
prefix to the 64-byte payload. A four-arm local smoke passes at 1,000 requests/s;
these are harness checks, not performance evidence. Hosted targeted capacity
checks follow to address the memtier saturation limitation.

An Intel Mac check in 34128161010 failed while draining the existing 16 MiB
pending-reply fixture. The other client’s PINGs had completed; the timeout was
in `reply(t, reader)`, and the captured server stack was in nonblocking socket
write. This string-only test does not exercise set compaction. The cause is
unresolved, and the failed run remains in
`bench/results/set-intel-pending-reply-failure-2026-09-07.json.gz`. A same-runner
baseline/candidate diagnostic repeats the exact test thirty times per arm and
three full suites per arm at package concurrency four, without widening deadlines.

The same-runner Intel diagnostic (34128949067) passes all thirty focused
repetitions per arm and three full suites per arm, using baseline 66f8ceb3 and
candidate 956188b9. The earlier timeout was not reproduced; its cause remains
unresolved. Passing repetitions do not establish that the earlier failure is fixed.
The final review makes the heap regression invoke compaction directly and require
both pending and completed states, preventing optional-interface skips.

Native scheduled run 34128714314 completes 36 arms against the same baseline.
All 960,000 requests balance: 745,260 complete, 214,740 admission drops, no
protocol failures or queue expirations. Each read returns 4,096 members.

| Workload | Offered/s | Completed/s median, baseline / candidate | Scheduled p99 ms, baseline / candidate | Drop %, baseline / candidate |
| --- | ---: | ---: | ---: | ---: |
| Set | 1,000 | 1,000 / 1,000 | 2.29 / 2.08 | 0 / 0 |
| Set | 2,000 | 2,000 / 2,000 | 3.11 / 2.79 | 0 / 0 |
| Set | 5,000 | 4,875.5 / 4,787.8 | 69.21 / 70.25 | 2.49 / 4.24 |
| Sorted set | 1,000 | 1,000 / 1,000 | 4.65 / 4.92 | 0 / 0 |
| Sorted set | 2,000 | 1,801.9 / 1,809.4 | 176.16 / 176.16 | 9.90 / 9.53 |
| Sorted set | 5,000 | 1,807.4 / 1,775.2 | 178.26 / 184.55 | 63.85 / 64.50 |

These read-only arms do not trigger churn compaction. They check ordinary-path
adoption and expose overload; they show no general throughput gain. Generator
CPU reaches 1.82 cores on its two-core allocation at the largest set offer, so
that saturation point still cannot be attributed solely to Keel. Public-host
variation remains uncontrolled. Matched ordinary workloads, the direct heap
regression and correctness tests support adopting the memory change; deployment
capacity and transient shadow-map memory remain separate limits. Raw results and
all Intel diagnostic logs are in
`bench/results/set-native-and-intel-diagnostic-2026-09-07.json.gz`.
