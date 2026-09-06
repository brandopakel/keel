# General-purpose Keel engineering and validation

Keel is the optimization target. A selected application is one compatibility
case, not the definition of the product or the only performance workload.
GoGIF remains unchanged during this work. Application-side improvements are
not counted as Keel improvements. Spending remains capped at $0.

## Work program

| Work | Implementation and evidence required | Status |
| --- | --- | --- |
| Baseline and profiles | Fixed seeds, exact executables, CPU/allocation/live-heap profiles, baseline RSS and growth with keys/clients | In progress |
| Broad performance | Read-heavy/balanced/write-heavy caches; value and working-set sizes; pipelines and connections; hashes, lists, sets, sorted sets, counters and sketches | In progress |
| Memory/CPU optimization | Change demonstrated Keel bottlenecks; retain baseline/candidate/Redis results, errors and regressions | Pending profiles |
| Persistence latency | Profile append, rewrite and large-key serialization; bounded work and acknowledgement-preserving changes | Pending |
| Operational validation | Sustained churn, TTL, eviction, slow clients, rewrite, disk errors, disconnect/reconnect, primary/replica restarts; overnight and multi-day reports | Pending |
| Replication development | Larger bounded snapshots, efficient large-key changes and durable resumable recovery, with explicit protocol/version compatibility | Pending |
| Availability design | Election/quorum, durable terms, fencing and acknowledgement semantics before automatic failover implementation | Pending design |
| Compatibility | Differential tests of supported RESP2 semantics, workload-driven SCAN/sorted-set gaps, client initialization and pipelining | Pending |

Embedding, partitioning, transactions and broad Redis compatibility require
their own supported contracts; a Redis-compatible cache does not imply every
Redis feature or arbitrary use-case compatibility.

## Measurement rules

- Compare the previous Keel and candidate with the same toolchain, workload,
  clients, persistence, memory limits and warmup. Redis is a separate comparison
  with matched settings and documented semantic differences.
- Run one server arm at a time. Rotate arm order and retain every repetition.
  Report per-workload ratios and spread instead of one overall speedup score.
- Report request correctness, errors, scheduled arrivals, dropped work and
  queueing alongside throughput and latency. Closed-loop throughput is not an
  offered-load capacity curve; pipelined batch latency is not per-command latency.
- Separate baseline process RSS, steady live heap, allocation rate, retained
  buffers and dataset growth. Estimated keyspace bytes are not total process RSS.
- Keep profiles out of timed comparative runs. Profiling changes scheduling and
  allocation sampling can be expensive.
- Use small and large values; uniform and skewed accesses; hit and miss mixes;
  stable and churning data; one and many clients; pipeline depths 1/16/64.
- Exercise AOF off/no/everysec/always independently, plus rewrite-under-load,
  slow storage and failures. Never improve a result by weakening its configured
  acknowledgement contract.
- Keep public CI smoke results distinct from local diagnostic runs, dedicated
  host measurements, deployment-network runs and overnight/multi-day evidence.
- An in-progress long run is not a pass. Preserve periodic progress and terminal
  success/failure, exact binary/source hashes, logs and recovery assertions.

The first milestone is a reproducible, measured improvement inside Keel with
correctness and recovery intact across this broader suite. Dedicated load/server
hosts and deployment filesystem tests require suitable available infrastructure;
account login alone does not establish a zero-cost compute allocation.
