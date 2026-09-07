# Bounded keyspace traversal and SCAN

Status: implemented candidate, September 6, 2026. Unreleased. The measurements
below were taken on a machine running three long soaks and are directional, not
capacity results; the end-to-end gate is not yet met. See
[the gate](#what-this-does-not-establish).

`persistence-replication-next.md` asks for a stable traversal abstraction shared
by rewrite, full synchronization and SCAN, and requires per-key memory and
mutation overhead at 100k and 1M keys before adopting one. This records the
design that was chosen, why, and what it measured.

## Why the obvious cursor is unavailable

Redis resumes a walk because its hash table exposes buckets: a SCAN cursor is a
bucket index visited in reverse-binary order, so a rehash cannot lose an entry.
Every Keel keyspace was a Go map, which exposes no buckets and randomises its
iteration order on every range. An offset into one is meaningless the moment the
range ends, so the cursor Redis uses cannot be ported.

That leaves four options, and each was costed before one was chosen.

| Option | Pause | Memory | Write path | Blast radius |
| --- | --- | --- | --- | --- |
| Snapshot key names per scan | O(N) at scan start | O(N) per live cursor | unchanged | small |
| Sorted index beside each map | O(1) | per-key index entry | +O(log n) per write | medium |
| Partition into fixed shards | O(N/shards) | none retained | +one hash per op | medium |
| Replace the map with a bucket-exposing table | O(bucket) | O(1) | unchanged | every store |

The partition was chosen. It is the only one of the first three that keeps both
the pause and the retained memory bounded, and unlike the fourth it does not
require hand-writing a hash table with its own rehash and correctness burden.

## What the partition buys

Each keyspace is `shardCount` maps, and a key's shard is a hash of its name, so
membership never changes while the key exists. That makes a shard index a usable
cursor, and it makes the cursor a small integer the client hands back verbatim.

The server therefore keeps **no per-cursor state at all**. Nothing has to expire
an abandoned cursor, nothing caps concurrent scans, and a client that walks away
mid-scan costs nothing. The lifetime, cleanup and retained-memory questions the
design note raised about a cursor mechanism do not arise, because there is no
cursor object to manage.

Because each shard is emitted exactly once, a key present for the whole walk is
returned **exactly once**. Redis's SCAN can return the same key more than once
after a rehash; this cannot. Keys created or removed mid-walk may or may not
appear, which is the same as Redis.

`COUNT` bounds keys *examined*, not keys returned, so `MATCH` and `TYPE` filter
what has already been paid for. A selective filter yields short or empty batches
with a cursor still to follow, so a client must stop on a zero cursor and not on
an empty reply. That is Redis's rule as well.

A cursor past the end of the keyspace reads as finished rather than as an error
or as a read of the wrong store, so a stale cursor cannot make SCAN lie.

## Measurements

Apple M4 Pro, 12 logical CPUs, macOS 26.5.1/APFS, Go 1.26.6. Three long
replication soaks were running throughout, so absolute figures are inflated and
only the paired comparisons carry meaning. Each arm is the median of three runs
in the same binary, back to back, so load moves both arms together.

Per-key memory, the same keys in one map against the partition:

| Keys | One map | Sharded | Difference |
| ---: | ---: | ---: | ---: |
| 100,000 | 3.29 MiB | 2.12 MiB | −12.3 bytes per key |
| 1,000,000 | 53.25 MiB | 38.07 MiB | −15.9 bytes per key |

The partition **costs no memory and saves it**, consistently across repetitions.
A Go map grows by doubling, and one large map carries the slack of its last
double alone; a thousand smaller maps round up individually and waste less in
aggregate. At a million keys that is a 28.5% reduction in the map's held bytes.

Mutation and lookup overhead, isolated store operations:

| Operation | Keys | One map | Sharded | Change |
| --- | ---: | ---: | ---: | ---: |
| get | 100,000 | 10.2 ns | 18.4 ns | +80% |
| get | 1,000,000 | 26.4 ns | 43.5 ns | +65% |
| set | 100,000 | 19.0 ns | 32.8 ns | +73% |
| set | 1,000,000 | 62.5 ns | 117.6 ns | +88% |

This is the real cost, and it is the hash of the key name that every store
operation now pays before it reaches a map that hashes it again. It is a large
proportion of a bare map operation and a much smaller proportion of a command:
a whole `GET` through dispatch, expiry and reply encoding measured 110 ns and a
whole `SET` 162 ns on a small keyspace, so at 100k keys the added hash is roughly
8% of a command rather than 80% of one.

The existing full walk, which `KEYS` and rewrite still take, is not made worse:

| Full key-name walk | One map | Sharded | Change |
| ---: | ---: | ---: | ---: |
| 100,000 keys | 0.64 ms | 0.74 ms | +15% |
| 1,000,000 keys | 7.79 ms | 7.91 ms | +1.5% |

That matters because rewrite begins by collecting every key name, so sharding
had to add a bounded walk without taxing the unbounded one it sits beside.

What the cursor buys against that walk:

| | 100,000 keys | 1,000,000 keys |
| --- | ---: | ---: |
| One `SCAN` call | 0.59 µs | 7.4 µs |
| Full key-name walk | 0.74 ms | 7.9 ms |

A SCAN call is bounded by one shard whatever the keyspace size: three orders of
magnitude below the walk, and it stays there as the keyspace grows.

## What this does not establish

- **The end-to-end gate is not met.** The per-operation figures are isolated
  store benchmarks. The authoritative number is the standard memtier harness
  comparing this candidate against its parent on a quiet machine, and that has
  not been run. Whether an 8% share of a command is acceptable for bounded
  enumeration is a judgement to make against that measurement, not this one.
- Every figure here was taken under three concurrent soaks.
- `shardCount` is fixed and untuned. It trades reply size and per-call pause
  against the per-store array; nothing here searched that space.
- Rewrite and full synchronization still take the whole key-name slice. Sharing
  this cursor with them is the follow-up that makes the abstraction shared
  rather than merely available, and it is deliberately not in this change
  because it collides with the rewrite work under review in PR #20.
- SCAN does not guarantee a consistent snapshot, and never can: keys added or
  removed during a walk may or may not be reported.
