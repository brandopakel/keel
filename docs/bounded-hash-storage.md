# Bounded map leaves for large hashes

Status: measured candidate awaiting integration and final review, September 7, 2026.

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

Two hosted matched runs compare the same pre-term baseline `a7c6600` against
this candidate using alternating arms on disjoint exposed CPU groups. Run
34153539090 covers nine cases at `ca86dff`; run 34155075225 repeats six cases at
the corrected accounting runtime `62d4285`, five 15-second repetitions each.
The corrected run has no client CPU warnings in any of its 60 arms:

| Workload | Paired throughput ratio, median (range) | p99 ms, baseline → candidate | RSS MiB, baseline → candidate |
| --- | ---: | ---: | ---: |
| Small string read | 0.991 (0.989–1.005) | 0.159 → 0.159 | 14.74 → 14.78 |
| Many one-field hashes | 1.005 (0.995–1.007) | 0.167 → 0.159 | 14.71 → 13.04 |
| Large-hash field read | 0.978 (0.964–0.986) | 0.159 → 0.167 | 38.01 → 40.26 |
| Large-hash mixed read/write | 0.972 (0.947–0.987) | 0.167 → 0.175 | 38.03 → 40.26 |
| Large-hash field write | 0.983 (0.967–1.014) | 0.167 → 0.175 | 36.12 → 40.28 |
| Large-hash delete/reinsert | 0.968 (0.963–0.997) | 0.159 → 0.167 | 30.02 → 30.55 |

The initial run likewise shows roughly 2–3% lower large-hash throughput.
Its ordinary small-read, many-client, pipeline-64 and small-hash median ratios
are 0.999, 1.002, 1.006 and 0.995. Every HGETALL arm in that run reports client
CPU pressure and is excluded from capacity conclusions. Public VM tenancy and
scheduling remain uncontrolled, and these runs predate corrected term guards.

The engineering tradeoff is explicit: accept the repeated roughly 2–3% active
large-hash throughput cost and larger 100k-field RSS to bound individual map
copies and release nearly all excess capacity after deep shrinkage. This is a
retention/maintenance improvement, not a general throughput or memory win.
Applications dominated by active large-hash lookups should assess that cost.
Raw reports, histograms in text form, host records and summaries are retained in
`bench/results/bounded-hash-matched-2026-09-07.json.gz`.

This candidate has not merged or been released. The frozen long soaks use their
original runtime and cannot validate it. Aggregate temporary reservations,
partially occupied key pages and filesystem handoff stalls remain separate work.
