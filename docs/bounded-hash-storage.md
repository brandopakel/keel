# Bounded map leaves for large hashes

Status: prototype awaiting matched adoption, September 7, 2026.

Small hashes keep their existing Go map. Above 192 fields, a hash routes fields
through a seeded hash-bit tree whose leaves each hold at most 192 fields.
Branches test distinct bits, limiting a path to 64 branches. Exact 64-bit hash
collisions use a chain of bounded leaves; worst-case lookup time in that chain
is not fixed. No hash or link is retained per field.

Growth splits one full leaf. Deletion rebuilds a leaf after substantial shrink,
drops empty branches and merges at most one small sibling pair. A delete never
copies the whole surviving hash. Once only one leaf remains it becomes the
ordinary small map again without copying. These are synchronous bounded local
operations: hashing long field names and multi-field commands can still pause
the loop. This does not make commands concurrent or guarantee a latency ceiling.

Hash grows from 24 to 32 bytes. Three local 10,000-collection measurements show
about eight extra bytes per empty, one-, eight- and 128-field hash. A million-field
population retains roughly 79.3–79.5 MB rather than the baseline's 91.4 MB. After
shrinking to 1,000 verified survivors it retains roughly 84–90 KB rather than
83.7 MB. The 100,000-field case grows slightly larger than baseline before shrink.
These are live-heap measurements, not RSS or a promise about every dataset.

Grow/delete diagnostics cost more CPU than the monolithic map. Their elapsed
times include population, deletion and explicit GC on a Mac running other tests
and frozen soaks, so they do not qualify throughput. An eight-arm, two-second
local smoke confirms the new 100,000-field read/mixed/write/churn workloads run
and verify their populations; its churn result is slower and needs matched
measurement. Adoption requires representative ordinary and large-hash workloads
on the same host with repeated alternating baseline/candidate arms.

Tests check exact field/value contents and cursor traversal through repeated
grow/shrink/reuse, random mutations, forced hash collisions, binary field names,
bounded leaf sizes and non-repeating branch bits. Rewrite tests delete most
fields while a cursor is active, regrow with new names, then verify exact values
and TTL through two replays. The first prototype's exhausted-iterator panic is
retained in the diagnostic logs; the corrected iterator passes these checks,
three focused race repetitions, the full local suite and vet.

The benchmark matrix adds `hash-field-read`, `hash-field-mixed`,
`hash-field-write` and `hash-field-churn`, each addressing fields inside one
100,000-field hash. The previous `hash` scenario uses many one-field hashes;
`large-hash-read` reads a whole 4,096-field hash. All remain useful and distinct.

Review follow-up: the indexed memory estimate now tracks retained leaf capacity
and node objects in O(1), updating the charge on splits, collision-chain growth,
shrinks, merges and removals. Payload lengths and calibrated allocator slack are
charged separately. Tests check accounting symmetry and measured heap after
half the fields are removed, before every leaf has compacted. This remains an
estimated keyspace budget rather than an exact allocator or RSS measurement.

The routing seed intentionally remains process-local and unpredictable. Unlike
probabilistic counters or filters, a hash stores exact field/value strings;
routing-tree shape and HGETALL order are not persisted semantics. Existing
rewrite/replay checks reconstruct new seeds and verify the exact logical data.
Changing to a known deterministic routing hash would weaken collision resistance
without improving replay correctness. Exact collision chains remain a documented
worst-case lookup limitation; this candidate bounds individual map capacity,
not every lookup's work under arbitrarily many exact 64-bit collisions.

This prototype has not merged or been released. The frozen long soaks use their
original runtime and cannot validate it. Aggregate temporary reservations,
partially occupied key pages and filesystem handoff stalls remain separate work.
