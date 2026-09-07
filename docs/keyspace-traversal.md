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

The count is the one tuning decision, and it was settled by measurement rather
than by picking a round number. It trades two costs that pull opposite ways.

Too many shards pays the fixed cost of a map holding almost nothing, over and
over. The first implementation used 1024, which under **Go 1.22 added 21.8 bytes
per key at 100,000 keys** — enough that the keyspace estimate stopped bounding
the real heap and `TestEstimateTracksRealHeap` failed on the Go 1.22 CI jobs.

That failure did not reproduce on Go 1.24 or later, whose maps are laid out
differently and where the same partition *saves* memory. **The floor in go.mod is
what a structure like this has to be sized against, not the newest toolchain**,
and a measurement taken only on the newest one is not evidence about the floor.
Measuring on Go 1.26 alone is what let 1024 look free.

| Shards | Go 1.22 estimate/heap at 100k, 8-byte values |
| ---: | ---: |
| 1024 | 0.825 — fails the 0.90 bound |
| 512 | 0.865 — fails |
| 256 | **0.925** |
| 128 | 0.950 |

256 was chosen: it clears the bound on the floor toolchain with margin, and it
halves the largest reply a single call can produce compared with 128. At the five
million keys `KeyNumberLimit` allows, a shard is about 19,500 keys, and that is
the worst case for both the reply and the pause.

## Measurements

Apple M4 Pro, 12 logical CPUs, macOS 26.5.1/APFS. Three long replication soaks
were running throughout, so absolute figures are inflated and only the paired
comparisons carry meaning. Each arm is the median of three runs in the same
binary, back to back, so load moves both arms together. Timings are Go 1.26.6;
memory is reported for both the floor and the current toolchain.

Per-key memory, the same keys in one map against the partition:

| Keys | Toolchain | One map | Sharded | Difference |
| ---: | --- | ---: | ---: | ---: |
| 100,000 | Go 1.22 | 3.75 MiB | 2.60 MiB | −12.0 bytes per key |
| 1,000,000 | Go 1.22 | 55.25 MiB | 40.75 MiB | −15.2 bytes per key |
| 100,000 | Go 1.26 | 3.29 MiB | 1.80 MiB | −15.6 bytes per key |
| 1,000,000 | Go 1.26 | 53.12 MiB | 38.02 MiB | −15.8 bytes per key |

At 256 the partition **saves** memory on both toolchains, because a Go map grows
by doubling and one large map carries the slack of its last double alone, while
many smaller maps round up individually and waste less in aggregate.

That isolated figure is not the whole story, and the difference is worth stating:
against the real dictionary, whose expiry table is not sharded and whose values
are separately allocated, the same change *adds* about 6.4 bytes per key at
100,000 keys on Go 1.22 (heap 11.99 MiB before, 12.63 MiB after). A benchmark on
a bare `map[string]int` is a guide to the partition, not a substitute for
measuring the structure that actually ships.

Mutation and lookup overhead, isolated store operations:

| Operation | Keys | One map | Sharded | Change |
| --- | ---: | ---: | ---: | ---: |
| get | 100,000 | 9.0 ns | 15.4 ns | +71% |
| get | 1,000,000 | 23.5 ns | 38.3 ns | +63% |
| set | 100,000 | 15.2 ns | 24.1 ns | +59% |
| set | 1,000,000 | 57.9 ns | 103.5 ns | +79% |

This is the real cost: a hash of the key name that every store operation now pays
before reaching a map that hashes it again. It is a large proportion of a bare
map operation and a much smaller proportion of a command — a whole `GET` through
dispatch, expiry and reply encoding measured 110 ns and a whole `SET` 162 ns, so
at 100k keys the added hash is roughly 6% of a command rather than 71% of one.

The existing full walk, which `KEYS` and rewrite still take, is unaffected:

| Full key-name walk | One map | Sharded | Change |
| ---: | ---: | ---: | ---: |
| 100,000 keys | 0.63 ms | 0.66 ms | +4.5% |
| 1,000,000 keys | 7.73 ms | 7.77 ms | +0.5% |

That matters because rewrite begins by collecting every key name, so sharding had
to add a bounded walk without taxing the unbounded one beside it.

What the cursor buys against that walk:

| | 100,000 keys | 1,000,000 keys |
| --- | ---: | ---: |
| One `SCAN` call | 1.9 µs | 27 µs |
| Full key-name walk | 0.66 ms | 7.77 ms |

A `SCAN` call is bounded by one shard whatever the keyspace size: nearly three
orders of magnitude below the walk, and it stays there as the keyspace grows.

## What this does not establish

- **The end-to-end gate is not met.** The per-operation figures are isolated
  store benchmarks. The authoritative number is the standard memtier harness
  comparing this candidate against its parent on a quiet machine, and that has
  not been run. Whether an 8% share of a command is acceptable for bounded
  enumeration is a judgement to make against that measurement, not this one.
- Every figure here was taken under three concurrent soaks.
- `shardCount` was sized against the Go 1.22 memory bound and the reply a single
  call can produce. It was not tuned against throughput, and the sweep above
  covered four values, not a search.
- A call emits a whole shard, so at the key limit a default `SCAN 0` can return
  about 19,500 keys where Redis would return about ten. Bounding that further
  would need a second level of hash bits inside a shard, which is possible
  without extra maps and is not done here.
- Rewrite and full synchronization still take the whole key-name slice. Sharing
  this cursor with them is the follow-up that makes the abstraction shared
  rather than merely available, and it is deliberately not in this change
  because it collides with the rewrite work under review in PR #20.
- SCAN does not guarantee a consistent snapshot, and never can: keys added or
  removed during a walk may or may not be reported.
