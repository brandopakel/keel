# Sorted-set score-map compaction

Status: candidate, September 7, 2026. A sorted set that has crossed 1,024
members tracks its high-water population lazily. Shrinking to a quarter starts
a replacement dictionary. Maintenance copies live skip-list nodes rather than
walking sparse Go map buckets, using the existing shared 1,024-work budget and
cooperative 1 ms target for TTL, lookup and collection maintenance.

The old score map remains authoritative until the walk finishes. Insertions and
score changes mirror into the replacement; removals delete from both. Before
removing or rescoring the cursor's node, the loop advances the cursor to a live
successor. Further substantial shrink restarts the replacement, and regrowth
cancels it. A walk that chases inserted nodes for twice its initial population
abandons the incomplete replacement. A later removal may retry; completion under
continuous adversarial churn is not guaranteed. Empty large maps and tracking
state are released. No logical change or persistence record is produced.

There is no per-member metadata. ZSet grows from 24 to 32 bytes; state allocates
only for large collections. A three-repetition, 10,000-collection local heap
diagnostic measures about eight additional bytes per empty, one-, eight- and
128-member collection. That is approximately 1.14%, 0.79%, 0.51% and 0.043% of
the respective measured baseline populations. Heap measurements are not RSS.

The 100,000-to-1,000-member regression reclaims 3,440,464 bytes. Three guarded
million-to-1,000 measurements reclaim a median 55,784,240 bytes. Each copies
exactly 1,000 survivors in 28 calls with a 37-node budget; the locally observed
worst slice is 13–16 microseconds. Those are diagnostics on a Mac with ongoing
soaks, not a dedicated-host latency guarantee. Map growth, hashing long member
names and individual commands can still exceed the time target.

Tests cover deletion/rescoring of the live cursor, insertions, equal and infinite
scores, regrowth, further shrink, empty-map reuse, bounded work under continuous
churn, small-collection overhead, and scheduled maintenance with exact rank,
score and two-file-read persistence/replay checks. Three focused race repetitions,
the full local suite and vet pass. Matched ordinary workload adoption and hosted
platform/filesystem validation are required before merging the candidate.

Hash map retention, aggregate temporary reservations and partially occupied key
pages remain separate work. Existing frozen long soaks validate their recorded
binary; this candidate needs its own integrated validation after adoption.
