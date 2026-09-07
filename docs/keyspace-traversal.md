# Stable keyspace traversal

Status: implementation under validation, September 7, 2026. This supersedes the
fixed 1,024-shard proposal in PR #21. The original proposal reduced average pause
size but could not stop inside a shard. Its approximately 4,900 keys per shard at
five million keys was an average, not a maximum.

## Representation

Each keyspace has pages of 64 stable slots. A slot contains the key name and its
typed entry value. A keyed, process-random 64-bit hash maps to a slot number;
full-hash collisions retain distinct slot numbers and verify the complete key.
Collisions are tested with an injected constant hash. They never merge keys.

Live entries do not move when the lookup map or page directory grows. Empty
slots are reused before allocating another page. Empty pages release their
backing allocation, and an empty store releases its directory and lookup map.
Entry metadata lives inline in pages, replacing per-entry allocations. This
avoids a separate string-keyed lookup table plus another full key header for
traversal. No unsafe runtime/map access is used.

Partially occupied pages and lookup-map capacity can retain memory after churn;
this representation does not imply an RSS ceiling. The keyspace memory estimate
and process memory must be assessed separately. Compaction cannot move live
slots casually because their positions are the public cursor contract.

## SCAN contract

The cursor encodes a store index and a stable slot number. It retains no server
iterator, copied key list, or per-client state. Abandoned cursors therefore need
no timeout or retained-memory cleanup. Cursors apply within one process; clients
restart a walk after reconnect/restart when continuity is required.

A key that remains in the same keyspace throughout a complete walk is returned
once. Newly created or deleted keys may or may not appear. There is no consistent
snapshot promise. Cross-type replacement is a deletion/new entry for this
contract; callers should tolerate duplicates as they do with Redis.

COUNT is a requested work budget, clamped to 1,024 work units per call. One unit
is a live slot examined or an empty page skipped; filters and vacant pages cannot
hide an unbounded traversal. One call visits at most one nonempty keyspace, so
the work/byte targets do not multiply across types. Stop only on cursor zero,
including when a filtered call returns no names.

Names have a 64 KiB processing target per call, including framing allowance. One
name larger than the target is handled alone to preserve progress. A one-ms
time target is checked between names. These are cooperative scheduling targets;
one oversized name, response encoding, runtime scheduling and GC can exceed the
time target. There is no real-time latency guarantee.

MATCH consumes at most 1,048,576 matcher transitions/class bytes per command.
Exhaustion returns an explicit error, never an incomplete successful result.
Use a simpler pattern or smaller COUNT. Matching is byte-based with Redis-style
wildcards; explicit empty MATCH matches only an empty key. TYPE values are
case-insensitive and an explicit empty/unknown type matches nothing. Expired
keys are filtered without changing their eviction scores or reaping keys during
SCAN. The fixed matcher budget is a documented Keel resource limit beyond
Redis's COUNT hint.

## Rewrite and full synchronization

A rewrite captures one slot high-water mark per keyspace. Startup allocation and
key enumeration do not scale with key count. Each advance fetches a bounded name
batch through the same traversal and serializes it in existing collection/key
slices. Changed keys are reconciled with deletion/replacement as before, including
keys inserted into previously visited vacancies or beyond the captured limits.
A registry replacement invalidates a walk explicitly rather than silently
traversing a different set of stores.

Protocol 2 snapshots are made from this incremental rewrite. Protocol 1 uses the
same small name batches to avoid an all-key name allocation, but still constructs
its legacy single response synchronously within the existing 8 MiB/100k-key
limits. Its wire format does not provide incremental snapshot responses; protocol
2 is the streamed alternative.

Individual large values, opaque collection images, file writes and final sync
still need separate latency work. Removing initial key enumeration does not
remove those stalls.

## Validation and adoption gate

Focused tests cover exact scan work limits, empty/filter/type cases, hash
collisions, stable slots across insertion/deletion/map growth, freed-page reuse,
large names, expensive patterns, frozen rewrite limits and registry invalidation.
Existing rewrite mutation/replay and replication tests exercise final-state
correctness using the new traversal.

Initial local Go 1.26.6 heap-accounting checks passed with estimate/live-heap
ratios 0.945, 1.019, 1.005 and 1.001 for 100k string keys with 8/64/512/4096-byte
values. These are accounting checks taken during active soaks, not performance
claims or a replacement for the complete memory comparison.

The first end-to-end matrix completed for `40fb7e5` versus `7d863b0`. It compared
the candidate using one Go compiler, fixed workloads, fresh servers, rotated arms,
three repetitions and disjoint server/generator logical CPUs on one Linux hosted
VM. The standard 24-case suite and nine memory cases retain binary hashes,
fixture hashes, native generator output, latency histograms and host metadata.
A hosted VM is not a dedicated physical machine or an application deployment.
Results must be reviewed per workload; a passing harness alone is not evidence
that the performance tradeoff is acceptable.


The [first matched run](https://github.com/brandopakel/keel/actions/runs/34095159507)
completed all 24 workload and nine memory cases, three repetitions per arm. Its
summaries, manifests, hashes and host details are retained in
`bench/results/traversal-first-matched-2026-09-07.json.gz`.

The 100k-key workload had candidate/baseline throughput ratios 0.961, 0.950 and
0.933; this is an observed regression requiring a tradeoff decision. Pipeline-16
ratios were 0.934, 1.026 and 0.950. Most other paired medians were within about
four percent. Large-list reads flagged generator CPU pressure in two repetitions
per arm. No aggregate speedup is claimed.

The 1 MiB workload's median p99 was 6.111 ms versus 41.215 ms, but both arms
showed the approximately 41 ms mode in individual repetitions and approximately
43 ms p99.9. That requires a longer targeted measurement; it does not establish
a causal regression or justify dismissing the tail difference. Million-key RSS
medians were 213.82 versus 215.82 MiB (about +0.9%); the empty-process medians were
7.42 versus 7.55 MiB. The VM exposes two cores with SMT: separate logical CPU
masks do not isolate physical core resources.

All integrated `cd2b740` PR CI jobs passed, including Go 1.26/stable on Linux and
macOS, race tests, Docker persistence/restart, Redis differential checks, native
Linux ARM64/Intel Mac recovery, and ext4/XFS checks. The matched comparison of
that revision against current develop `db4fd00` is still running. The adoption
review remains open pending that result and the targeted tail investigation.
