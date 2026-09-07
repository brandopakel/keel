# Reclaim lookup tables after partial key churn

Stable key pages release empty payload pages, but deleting most keys does not
shrink the hash-to-slot Go map. A 100,000-key store shrunk to 1,000 keys retained
about 2.33 MB of unnecessary lookup capacity in both typed string and collection
stores. The same regression now recovers those bytes through maintenance.

When a lookup table with a peak of at least 1,024 hashes shrinks to one quarter
of that peak, build a smaller lookup across bounded traversal turns. The source
map remains authoritative until completion. Insertions and deletions mirror
current hash heads into the replacement, including collision-head promotion,
reused slots and keys beyond the captured traversal end. Cancel if the table
regrows beyond half its peak; clear both tables when the store empties.

Compaction keeps page slots, collision lists and active SCAN/rewrite cursors in
place. It adds two fields per store, with no per-key metadata or persistence
format change. TTL and lookup rebuilding share the existing 1,024-work-unit
maintenance budget and cooperative one-millisecond target. Their precedence
alternates to avoid one family starving the other. Maintenance remains safe
on replicas and while immutable worker appends are pending.

The reproduced retention check, interleaved mutation/collision checks, unchanged
slot positions and full scans, bounded start allocation, regrowth cancellation
and empty-store cleanup pass. Full local tests/vet and focused race pass. Raw
before/after evidence is in `bench/results/lookup-compaction-2026-09-07.txt`.
Hosted integration, differential and workload adoption remain pending.

Old and new lookup tables coexist while rebuilding. Completion may take many
maintenance ticks; no completion-time or single-command latency SLA is implied.
Sparse page directories and nonempty payload pages retain their stable layout;
this change does not compact pages or collection-internal maps.

The hosted a582361 comparison in 34114141204 passed broad differential and
operational validation and completed three ten-second repetitions of small-read,
write, TTL and 100k-working-set workloads plus the nine-case memory matrix.
Paired median throughput ratios were 0.998, 1.012, 1.000 and 1.003; measured
p99 stayed the same or lower. No generator CPU warnings occurred. Million-key
RSS was 215.56/217.78 MiB; this static workload does not demonstrate memory
savings. The demonstrated benefit is releasing unused lookup capacity after
partial deletion, covered separately by the retained-heap regression. Raw hosted
measurements are in `bench/results/lookup-matched-2026-09-07.json.gz`.

The integrated maintenance loop includes PR #36's review correction: traversal
stops when the shared work/deadline budget is reached, rather than invoking
no-op callbacks for every remaining store. Mutation/cursor/maintenance tests
pass after this integration; final hosted checks and review remain required.
