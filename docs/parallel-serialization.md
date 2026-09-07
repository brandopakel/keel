# Parallel rewrite and snapshot serialization

Status: costed and **not recommended yet**, September 7, 2026. Nothing here is
implemented. This exists because the question was worth answering properly
rather than left as a vague "we could thread that."

## What is already asynchronous

More than the "single-threaded server" description suggests. Only one thing on
this list is not off the event loop:

| | Where it runs |
| --- | --- |
| Socket read and write, RESP parsing | `-io-threads N` worker pool, with the loop thread taking a share rather than supervising idle |
| AOF append, and `always` fsync | append worker, handed an immutable batch |
| `everysec` fsync | its own worker |
| Command execution during an append | overlapped by `-aof-concurrent-append`, replies gated by the durable prefix |
| Rewrite | sliced across loop cycles, but the work runs **on** the loop |
| **Keyspace mutation** | **the loop, serially** |

## Why sharding does not make execution parallel

The keyspace is now partitioned, and partitioning is the usual precondition for
parallel execution, so the question follows naturally. It is a precondition and
nothing more. Five things serialize execution regardless of how the data is
split:

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

The last is the decisive one, and it is worth being precise about because it
also governs the idea that *is* worth considering.

## The one real opportunity

Rewrite and snapshot serialization is CPU work over independent shards, which is
the shape that parallelizes. Better still, the correctness argument is already
made: any key written during a rewrite is marked dirty and re-emitted from its
current value at the end, so a worker that reads a stale or half-written value
produces bytes that are later replaced. **Logical correctness is not the
blocker.** The blocker is only that touching a Go map while the loop writes it
aborts the process.

Three ways round it, and their costs are not close.

### A. A lock per shard

`RWMutex` on each shard, read-locked by workers and write-locked by the loop.

Simple, and wrong for this. An uncontended `RLock`/`RUnlock` pair is on the order
of 20 ns; a whole `GET` through dispatch, expiry and reply encoding measures
110 ns. That is a permanent tax approaching a fifth of the read path, paid by
every client on every command, to make a background job finish sooner.

### B. Copy each shard under the loop, serialize it on a worker

No hot-path lock at all: the loop copies one shard, hands the copy over, and
keeps going.

It works cleanly for strings, where a copy is a slice of pointers to immutable
values. It does not for collections: copying a hash or a sorted set means
copying its contents, which is the same O(elements) work the serialization was
going to do, moved rather than removed. Since large collections are exactly the
case where a rewrite is slow, this helps least where it is needed most.

### C. Serialize from the log instead of the keyspace

Already what protocol 2 does. Its snapshot streams the rewritten AOF rather than
walking the keyspace, so for snapshots this problem is solved and the remaining
walk is the rewrite itself.

## Recommendation: not yet, and measure first

Not because it cannot be done, but because the benefit has not been shown to
exist.

**The rewrite does not stall the server.** It is sliced across loop cycles with a
budget of 2048 keys, 1 MiB or 1 ms per slice, and the walk now holds a shard of
names rather than the keyspace. Parallelism would make it *finish sooner*, not
stop it blocking — it does not block.

The one place finishing sooner has a concrete value is the ceiling: rewrite
refuses above a million keys, as a fail-fast against the 30-second duration and
100,000-key dirty budgets. If the goal is to raise that ceiling, parallelism is
an expensive way to buy it, and there are cheaper ones to try first:

- Where does rewrite time actually go — traversal, RESP encoding, or writes? No
  profile has been taken. It is possible the encoding is a small share, in which
  case parallelizing it buys almost nothing.
- The slice budgets and the abort window are constants that were chosen, not
  derived. Raising them may lift the ceiling for the cost of a longer tail.
- The dirty-key budget is what actually fails on a hot keyspace, and no amount
  of parallel serialization changes how many keys are being written meanwhile.

**Do the profile before buying anything.** If it shows serialization dominating,
option B for strings is the shape to take, because it is the only one that puts
no cost on the read path — and its weakness for collections can be measured
rather than assumed.

## What would change the answer

- A profile showing RESP encoding is most of rewrite time.
- A deployment that needs more than a million keys per node, where the ceiling
  is the binding constraint rather than a theoretical one.
- Collections becoming copy-on-write, which would remove option B's weakness by
  making a shard copy cheap for every type rather than only for strings.
