# Parallel rewrite and snapshot serialization

Status: architecture options, September 7, 2026. Parallel keyspace serialization
and command execution are not implemented. Adoption requires ownership and
allocation contracts plus matched measurements against the current runtime.

## What is already asynchronous

Command execution and mutable keyspace state have one event-loop owner.
Offloading is conditional on configuration and admission:

| | Where it runs |
| --- | --- |
| Socket read and write, RESP parsing | `-io-threads N` worker pool, with the loop thread taking a share rather than supervising idle |
| AOF append, and `always` fsync | optional append worker, handed an immutable batch |
| `everysec` fsync | its own worker |
| Command execution during an append | bounded admitted runs overlap with `-aof-concurrent-append`; other runs drain the barrier. Replies follow the configured persistence boundary, not always a durable prefix |
| Rewrite | sliced across loop cycles, but the work runs **on** the loop |
| Bulk snapshot preflush | worker; dirty-tail reconciliation and final filesystem handoff remain on the loop |
| **Keyspace mutation** | **the loop, serially** |

## Why sharding does not make execution parallel

Lookup tables are sharded internally. That is not an application partitioning
feature or a concurrency protocol. Parallel execution must address these
shared contracts:

1. **The AOF is one totally ordered log.** Parallel execution needs a
   deterministic record order that replay reproduces exactly. Per-shard logs
   change recovery, rewrite and replication; a sequencer puts the serialization
   back at the point you removed it from.
2. **Memory accounting is global.** `memUsed`, `TotalMemUsed` and
   `EnforceLimits` are single counters consulted on every write.
3. **Eviction is global.** It chooses a victim across the whole keyspace, and
   can pick one in a shard another thread is mid-write on.
4. **Multi-key commands span shards.** `MSET`, `MGET`, and anything
   transactional later, need multi-shard locking with an ordering discipline.
5. **Go maps are not safe for concurrent read and write.** Not merely racy: the
   runtime aborts the process. Every shard would need a lock taken on every
   `GET`.

All five require a design; mutable-value ownership also governs background
serialization.

## The one real opportunity

Rewrite encoding may be parallelizable after a safe snapshot is captured.
Dirty reconciliation can repair a coherent earlier value by replacing it with
the final value. It cannot repair a data race, torn header, invalid pointer,
panic or malformed record from concurrently reading mutable state. Every worker
input needs an immutable snapshot or explicit ownership protocol, with memory
reserved before capture and bounded queues, cancellation and cleanup.

Three ways round it, and their costs are not close.

### A. A lock per shard

`RWMutex` on each shard, read-locked by workers and write-locked by the loop.

Locks can protect access, but long serialization reads can block writers.
Isolated lock and dispatch timings do not establish an end-to-end throughput
cost. Measure contention, normal command latency and memory before choosing
this design.

### B. Copy each shard under the loop, serialize it on a worker

No hot-path lock at all: the loop copies one shard, hands the copy over, and
keeps going.

It works cleanly for strings, where a copy is a slice of pointers to immutable
values. It does not for collections: copying a hash or a sorted set means
copying its contents, which is the same O(elements) work the serialization was
going to do, moved rather than removed. Since large collections are exactly the
case where a rewrite is slow, this helps least where it is needed most.

### C. Serialize from the log instead of the keyspace

Protocol 2 retains an immutable rewritten-file descriptor and streams its byte
ranges after rewrite handoff. This avoids a second mutable-keyspace walk; it
does not eliminate snapshot construction or filesystem finalization costs.

## The measurement this asked for

Taken since: a CPU profile of one rewrite over a million keys, and rewrite
duration at three sizes. It settles the question rather than leaving it open.

Where rewrite CPU goes:

| | share of rewrite CPU |
| --- | ---: |
| `write` syscalls | 52% |
| RESP serialization (`emitRewriteKey`, `appendCommand`, `appendBulkString`) | 35% |
| Traversal (`KeyspaceWalk.Next`, `Dict.ScanUntil`) | 11% |

And the figure that matters more: over a 4.5-second rewrite the process was **on
CPU for 13% of the wall clock**. The rest was waiting on the log's writes and
syncs. So serialization is 35% of 13% - roughly **5% of rewrite wall time**.

Parallelizing 5% of the work caps the speedup near 1.05x however many workers are
used, and the price was a lock on every `GET` or a copy that for collections
moves the cost rather than removing it. That is not a close call.

The dominant cost is the write syscall against a single ordered file, which is
the one part that cannot be parallelized at all.

## Recommendation: no, on the measurement rather than on the argument

Parallel serialization has not demonstrated an adoption benefit. Current
traversal walks bounded stable slots; large records stream in at most 64 KiB
fragments. The rewrite uses cooperative key, byte and 1 ms targets, but opaque
image construction, writes, final dirty-tail sync, rename and directory sync
can still block command execution. See [resource limits](rewrite-resource-limits.md),
[opaque records](opaque-rewrite-records.md) and
[matched preflush measurements](rewrite-preflush.md). Shorter rewrite duration
and lower command stalls are distinct outcomes to measure.

Finishing sooner may also help the snapshot key ceiling, 30-second
duration limit and dirty-name count/byte budgets. Compare alternatives rather
than assuming which one is cheaper:

- Profile traversal, capture, encoding, writes and finalization separately on
  the target workload. Existing preflush measurements do not establish that
  encoding dominates every dataset.
- One such profile exists and is a starting point rather than an answer. On a
  million small strings, before the preflush worker moved bulk writes off the
  loop, rewrite CPU was 52% `write` syscalls, 35% RESP encoding and 11%
  traversal - and the process was on CPU for 13% of the rewrite's wall clock,
  the rest waiting on the log. Encoding was therefore about 5% of wall time on
  that dataset and that runtime. Both caveats matter: moving writes to a worker
  changes the mix, and a dataset of large collections would encode far more per
  key than a small string does.
- The slice budgets and the abort window are constants that were chosen, not
  derived. Raising them may lift the ceiling for the cost of a longer tail.
- Faster completion may reduce dirty-key accumulation. Test that effect with
  controlled write mixtures instead of assuming the dirty budget is invariant.

Compare any candidate against current bounded streaming and preflush with the
same host, durability, dataset and offered load. Retain overload, allocation
peaks, errors and failed attempts. Test mutation, expiry, replacement,
cancellation, storage failures and crash recovery while work is outstanding.

## What would change the answer

- A profile showing RESP encoding is most of rewrite time on the target
  workload. The one recorded above says otherwise for a million small strings on
  the pre-preflush runtime, which is one dataset and one runtime, not a
  conclusion about every rewrite.
- A deployment that needs more than a million keys per node, where the ceiling
  is the binding constraint rather than a theoretical one.
- Collections becoming copy-on-write, which would remove option B's weakness by
  making a shard copy cheap for every type rather than only for strings.
