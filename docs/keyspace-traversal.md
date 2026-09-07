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

## Choosing the shard count

The count is the one tuning decision, and it was settled by measurement. It
trades two costs that pull opposite ways.

Too many shards pays the fixed cost of a map holding almost nothing, over and
over. Too few makes a shard a large piece to take whole, because a call emits one
and cannot stop inside it.

The first implementation used 1024 and CI rejected it — not on the newest
toolchain, but on the Go 1.22 floor `go.mod` then declared, where it cost **21.8
bytes per key at 100,000 keys** and the keyspace estimate stopped bounding the
real heap. Go 1.24 replaced the map implementation and the cost disappeared, so
the number a measurement gives depends on which toolchain took it, and the floor
is what a structure like this has to be sized against.

| Shards | Go 1.22 estimate/heap at 100k, 8-byte values |
| ---: | ---: |
| 1024 | 0.825 — fails the 0.90 bound |
| 512 | 0.865 — fails |
| 256 | 0.925 |
| 128 | 0.950 |

That constraint is now gone. Raising the floor to Go 1.25, the oldest release
still receiving security fixes, means every supported toolchain has the newer map
layout, and 1024 measures 0.982 on the floor and 0.980 on current Go. **1024 is
therefore what the floor allows rather than what it forces**: while `go.mod`
claimed 1.22 this had to be 256, at four times the reply and pause per call.

At 1024, the five million keys `KeyNumberLimit` allows come to about 4,900 per
shard, and that is the worst case for both the reply and the pause.

## Measurements

Apple M4 Pro, 12 logical CPUs, macOS 26.5.1/APFS. Three long replication soaks
were running throughout, so absolute figures are inflated and only the paired
comparisons carry meaning. Each arm is the median of three runs in the same
binary, back to back, so load moves both arms together. Timings are Go 1.26.6;
memory is reported for both the floor and the current toolchain.

Per-key memory, the same keys in one map against the partition:

| Keys | Toolchain | One map | Sharded | Difference |
| ---: | --- | ---: | ---: | ---: |
| 100,000 | Go 1.25 | 3.29 MiB | 2.14 MiB | −12.1 bytes per key |
| 1,000,000 | Go 1.25 | 53.23 MiB | 37.99 MiB | −16.0 bytes per key |
| 100,000 | Go 1.26 | 3.29 MiB | 2.13 MiB | −12.2 bytes per key |
| 1,000,000 | Go 1.26 | 53.21 MiB | 37.94 MiB | −16.0 bytes per key |

The partition **saves** memory on both supported toolchains, because a Go map
grows by doubling and one large map carries the slack of its last double alone,
while many smaller maps round up individually and waste less in aggregate.

That isolated figure is not the whole story. Against the real dictionary, whose
expiry table is not sharded and whose values are separately allocated, the
estimate/heap ratio moves from 0.975 to 0.982 — the accounting still bounds the
heap, which is what the test is for. A benchmark on a bare `map[string]int` is a
guide to the partition, not a substitute for measuring the structure that ships.

Mutation and lookup overhead, isolated store operations:

| Operation | Keys | One map | Sharded | Change |
| --- | ---: | ---: | ---: | ---: |
| get | 100,000 | 9.8 ns | 18.0 ns | +84% |
| get | 1,000,000 | 26.3 ns | 41.8 ns | +59% |
| set | 100,000 | 15.4 ns | 27.7 ns | +80% |
| set | 1,000,000 | 58.2 ns | 107.6 ns | +85% |

This is the real cost: a hash of the key name that every store operation now pays
before reaching a map that hashes it again. It is a large proportion of a bare
map operation and a much smaller proportion of a command — a whole `GET` through
dispatch, expiry and reply encoding measured 110 ns and a whole `SET` 162 ns, so
at 100k keys the added hash is roughly 7% of a command rather than 84% of one.

The existing full walk, which `KEYS` and rewrite still take, is close to
unchanged, and unchanged at the size where it matters:

| Full key-name walk | One map | Sharded | Change |
| ---: | ---: | ---: | ---: |
| 100,000 keys | 0.72 ms | 0.79 ms | +11% |
| 1,000,000 keys | 8.06 ms | 8.22 ms | +2% |

That matters because rewrite begins by collecting every key name, so sharding had
to add a bounded walk without taxing the unbounded one beside it.

What the cursor buys against that walk:

| | 100,000 keys | 1,000,000 keys |
| --- | ---: | ---: |
| One `SCAN` call | 0.56 µs | 6.9 µs |
| Full key-name walk | 0.79 ms | 8.22 ms |

A `SCAN` call is bounded by one shard whatever the keyspace size: three orders of
magnitude below the walk, and it stays there as the keyspace grows.

## What this does not establish

- **The end-to-end gate is not met.** The per-operation figures are isolated
  store benchmarks. The authoritative number is the standard memtier harness
  comparing this candidate against its parent on a quiet machine, and that has
  not been run. Whether an 8% share of a command is acceptable for bounded
  enumeration is a judgement to make against that measurement, not this one.
- Every figure here was taken under three concurrent soaks.
- `shardCount` was sized against the memory bound on the floor toolchain and the
  reply a single call can produce. It was not tuned against throughput, and the
  sweep covered four values, not a search.
- A call emits a whole shard, so at the key limit a default `SCAN 0` can return
  about 4,900 keys where Redis would return about ten. Bounding that further
  would need a second level of hash bits inside a shard, which is possible
  without extra maps and is not done here.
- Rewrite and full synchronization still take the whole key-name slice. Sharing
  this cursor with them is the follow-up that makes the abstraction shared
  rather than merely available, and it is deliberately not in this change
  because it collides with the rewrite work under review in PR #20.
- SCAN does not guarantee a consistent snapshot, and never can: keys added or
  removed during a walk may or may not be reported.
