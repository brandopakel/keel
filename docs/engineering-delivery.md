# Engineering delivery — September 4, 2026

This tracks the implementation following [the engineering review](engineering-review-2026-09-04.md).
The original review is historical evidence, not a list of unfixed bugs.

## 1. Crash, persistence, and accounting fixes

Implemented and regression-tested:

- Reject unaffordable Morris dimensions/probabilities and overflowing Cuckoo reservations.
- Validate binary restore dimensions, array sizes, registers, and structure state before allocating.
- Validate RESP terminators and negative lengths; grow array argument storage with received data.
- Validate a whole Morris batch before mutation; preserve Cuckoo RNG state on a failed insertion.
- Repair a torn AOF tail after preserving it, before appending again; stream replay command by command.
- Stop without staged success replies after AOF write/sync failure; sync idle dirty logs under everysec.
- Reject AOF on benchmark net modes. Exit promptly on internal server failures or invalid configuration.
- Account for integer value changes and TTL metadata; enforce eviction after a complete command.
- Keep historical replay independent of current expiry time, then expire keys before enforcing limits.
- Log startup expiry/eviction removals before new writes and lazy-expiry removals before key recreation.
- Preserve TTL across types, AOF replay, rewrites, and in-place mutations.

## 2. Bounded connection and rewrite work

Implemented:

- Nonblocking partial writes with read/write interest switching on epoll and kqueue.
- Persistent reply ownership across event-loop turns; tests cover ordered replies amid other traffic.
- Connection count, 16 MiB incomplete input, 64 MiB output, and 256 MiB aggregate retained-buffer limits.
- Close stalled partial requests/replies after 30 seconds without progress; maintenance runs about once per second.
- At most 128 ready descriptors per cycle; one bounded read per ready connection.
- Rewrite slices target 2048 keys, 1 MiB, or 1 ms, including dirty-key reconciliation.
- Rewrite admission at one million keys, abandonment at 30 seconds or 100,000 dirty keys, retry cooldown.
- Wakeup-based shutdown, connection cleanup, AOF close error propagation, and a five-second process backstop.

Limits still requiring engineering if workloads hit them:

- Snapshot enumeration and most individual-key serialization remain synchronous. Large-list rewrites now yield between bounded chunks, restarting on mutation.
- AOF appends, rewrite writes/finalization and `always` sync remain synchronous; slow storage can stall commands.
- `everysec` now uses one background sync worker, with error propagation and descriptor lifecycle fences.
- Response encoding allocates before checking its limit. Parsing and command execution require transient memory.
- Whole-key commands and large computations remain proportional to their input. These are not hard latency bounds.
- There is no per-client rate limit or CPU budget, nor fairness guarantee against expensive-command traffic.

These limits are explicit alpha constraints, not completed hard real-time scheduling work.

## 3. A useful integration path

Implemented:

- SET NX/XX/GET/KEEPTTL/EX/PX/EXAT/PXAT; INCRBY, DECR, and DECRBY.
- EXPIRE/PEXPIRE/EXPIREAT/PEXPIREAT NX/XX/GT/LT and PERSIST for every key type.
- MEMORY USAGE for hashes and lists; MEMORY STATS per-type estimates.
- ZRANGE rank ranges with REV/WITHSCORES; LTRIM with TTL and AOF behavior.
- Hash/list dumps; KEL1 checksummed dump envelope, with legacy readers retained.
- Connection-local AUTH, environment-based secret configuration, localhost default binding.
- A tested redis-py 5.2.1 cache/analytics example, explicit RESP2 and nontransactional pipelines.
- README integration boundaries, private-network/TLS proxy/container/AOF guidance and diagnostics.

Missing Redis commands remain unsupported. This is a selected compatibility contract, not a drop-in Redis replacement.

## 4. Alpha preparation and evidence

- License copied exactly from the project's `chore/licence` branch; third-party notices retained.
- Four candidate archive targets: Linux/macOS × amd64/arm64, including license, notices, docs and examples.
- `scripts/build-alpha.sh` builds local candidates and checksums without publishing.
- Release workflow gates builds on native Linux/macOS tests, race detection, and vet, and marks releases prerelease.
- Main CI includes Linux/macOS, Go floor/current, restore fuzz smoke, and a pinned Python client integration.
- Fixed live-connection memory measurement, exact process ownership, stale memtier output, silent benchmark omissions,
  unsafe broad process termination, and the benchmark workflow's obsolete Go version.

Local validation on Darwin arm64, Go 1.26.6:

- Full Go suite and race detector pass. Timing thresholds are assessed without race instrumentation; state assertions run in both.
- `go vet ./...` passes; all four target binaries compile.
- Real TCP tests exercise authentication, AOF repair over two restarts, shutdown, slow readers,
  reply ordering, and an injected file-size failure that must not receive a success reply.
- Restore fuzzing: approximately 725,000 executions in ten seconds without failure.
- Documented redis-py example passes against an authenticated AOF-enabled process.
- Repeated corrected idle-connection memory measurement is retained in
  [CSV](../bench/results/reliability-live-memory.csv) and [metadata](../bench/results/reliability-live-memory.metadata.json).

At 500 live idle connections, median RSS growth per connection across three fresh-process runs was
0.832 KiB for Keel's event loop, 46.656 KiB for its net variant, and 18.144 KiB for the installed Redis.
This supports an idle-connection memory advantage in this local experiment. It does not establish
cache-workload performance, operational superiority, or measurements on another platform.

Publication and native CI results must be recorded separately from local validation.
No outside user adoption or production validation is implied by candidate archives.

## 5. Demand-led scaling decision

Defer embedding, partitioning, and replication until a pilot identifies the actual constraint.

| Observed need | Engineering commitment | Evidence required first |
| --- | --- | --- |
| Avoid a service boundary inside a Go process | Instance-owned stores/configuration, concurrency/lifecycle API, stable public package | Application trace showing IPC/deployment is the bottleneck |
| More capacity or independent tenants | Routing, stable hashing, migration, multi-key behavior and per-partition budgets | Key distribution, working-set size, skew, resharding expectations |
| Survive node failure | Replication offsets/log transport, full synchronization, failover/fencing, consistency contract | Explicit RPO/RTO, failure model, durability and operational requirements |

The [pilot plan](pilot-plan.md) collects these inputs. Implementing all three speculatively would
create three products without establishing which one users need.

## September 5 follow-up (working tree, not yet released)

- Restored active expiry in the production event loop and added a real-server idle-expiry regression.
- Background everysec fsync, sticky failure reporting, and close/rewrite descriptor fences.
- Large-list rewrite chunks (256 elements / approximately 64 KiB), with mutation/replay tests.
- Repeated mixed-traffic tail-latency harness with independent scheduled probes and raw errors.
- Benchmark CI now uses the current memory columns and medians, fails on missing memtier or
  failed latency runs, retains artifacts on failure, and stops smoke servers by exact PID.
- Wider scaling acceptance criteria are in [async-scaling.md](async-scaling.md).

External application pilots, replication, partitioning and embedding remain incomplete.
The preceding implementation bullets do not establish a hard latency or data-loss bound.

External testing deliberation: adopt a layered CI/controlled-host/pilot strategy;
evaluate managed k6 or AWS load orchestration against RESP support, raw export,
reproducibility and cost. Do not move all correctness and fault tests into a hosted
load service. The detailed selection gates and first experiment are included in
[the updated plan](async-scaling.md#external-testing-and-benchmarking-decision).

Follow-up local validation: full Go suite, race suite and vet passed; nine baseline
and nine candidate latency smoke runs completed without request errors. The
[latency report](latency-smoke-2026-09-05.md) records the unchanged everysec p99 and
measurement limits. Expanded Linux benchmark CI has not run for this working tree.
The installed memtier tool also completed all five scenarios with the corrected
nested-percentile parser. Raw memtier JSON/logs are now retained rather than deleted;
missing metrics fail instead of silently becoming zero. Benchmark YAML and shell
syntax checks pass. These checks do not replace the pending controlled-host runs.

## Next implementation increment (working tree)

This supersedes the earlier “replication and fully asynchronous appends unfinished”
status with narrower, implemented experimental contracts:

- `-aof-async-append`: one worker batch and a reply/command barrier; readiness
  backpressure, write/sync failure propagation, pipeline/restart regressions.
  Concurrent command execution during disk appends remains future work.
- `-replication-feed` / `-replicaof`: authenticated bounded canonical full/delta
  replication, epoch/offset checks, checksum, read-only/stale gates and manual
  promotion after external fencing. See [exact limits](replication-alpha.md).
- Bencher BMF export and explicit publish wrapper; k6 RESP framing and scheduled
  load; AWS DLT Locust archive builder. [Adapter instructions](../bench/external/README.md).
- Actual local k6 and Locust smoke runs pass against an authenticated AOF process.
  BMF export, framing tests and AWS package generation are validated locally.

Hosted Bencher publication, controlled cloud runs, AWS provisioning and real pilots
remain pending the owner's account/region/budget/application inputs. No provider
execution or user adoption is implied by a locally tested adapter.
Local follow-up validation now also covers primary outage/stale-read rejection and
clean manual promotion after stopping the old writer. Full Go tests and vet pass;
race results and native CI are tracked for the final source revision separately.
