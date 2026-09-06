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
| Memory/CPU optimization | Direct typed RESP command parsing; avoid copying nonnumeric values during integer classification; retain matched results | Implemented; comparative validation running |
| Persistence latency | Profile append, rewrite and large-key serialization; bounded work and acknowledgement-preserving changes | Pending |
| Operational validation | Sustained churn, TTL, eviction, slow clients, rewrite, disk errors, disconnect/reconnect, primary/replica restarts; overnight and multi-day reports | Pending |
| Replication development | Larger bounded snapshots, efficient large-key changes and durable resumable recovery, with explicit protocol/version compatibility | Pending |
| Availability design | Election/quorum, durable terms, fencing and acknowledgement semantics before automatic failover implementation | Pending design |
| Compatibility | Seeded Redis reply/state differential, rewrite and two crash/restarts; canonical counters; SETEX/PSETEX | Initial suite passes; wider command surface remains |

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

## Run the broad suite

Build the baseline and candidate from clean commits with the same Go toolchain
and build flags. Supply the installed Redis and native memtier executables. The
current native harness is tested with memtier 2.5.1; CI builds its pinned source
archive and verifies its SHA-256. Redis's actual binary hash/version are recorded
because the local and Ubuntu package versions can differ.

```sh
go build -trimpath -o dist/keel-candidate ./cmd/keel
python3 bench/run-general.py --candidate dist/keel-candidate \
  --baseline /path/to/keel-baseline --redis /path/to/redis-server \
  --out dist/general/throughput --reps 3 --seconds 5
python3 bench/summarize-general.py dist/general/throughput
python3 bench/run-general.py --candidate dist/keel-candidate \
  --baseline /path/to/keel-baseline --redis /path/to/redis-server \
  --suite memory --out dist/general/memory --reps 3
python3 bench/summarize-general.py dist/general/memory
```

The standard suite has 22 scenarios: read/balanced/write caches, 64 B through
1 MiB values, misses, skew, TTL writes, 1/16/256 connections, pipelines 1/16/64,
100k keys, hashes, sets, rankings, queues, counters, HLL, a large list and
reconnections. The memory suite reaches one million keys and holds verified idle
connections open while sampling. A control connection is additional to the
reported workload/idle client count.

Every arm starts a fresh loopback server and uses the same seeded fixture.
Comparative runs disable authentication for all arms; the differential and
operational tests use authenticated servers. Baseline and loaded RSS are recorded
separately from RSS during traffic. Native JSON, HDR histograms, GC logs,
telemetry and tool/build provenance are retained. A failure remains a failure;
fresh output directories prevent accidentally overwriting it with a later pass.

For matching persistence comparisons, add `--policy no`, `--policy everysec`
or `--policy always`. Add `--worker` only when comparing the worker mode on both
Keel arms; Redis does not implement Keel's worker flag. Automatic rewrites are
disabled during these isolated throughput comparisons. Use the tail/operational
harnesses to exercise rewrites and recovery separately.

These cases are deliberately reproducible, not exhaustive. The TTL throughput
case measures TTL metadata writes with a 300-second expiry, not an expiry storm.
The collection cases initially use small collections; the large-list case covers
one larger response. Dense, low-cardinality HLL storage is a known Keel memory
cost. Broader collection cardinalities, mixed tenants and offered-load sweeps are
additional dimensions, not implied by a passing 22-case run. All load/server
processes currently share a host. Native memtier reduces generator overhead, but
closed-loop traffic can conceal overload queueing. Report its CPU warnings and
do not interpret short-run p99.9 as a stable deployment SLO.

## Profiles and operational evidence

```sh
python3 bench/run-general.py --candidate dist/keel-candidate --profiles \
  --cases cache-read-64,cache-write-64,hash,large-list-read \
  --out dist/general/profiles --reps 1 --seconds 10
python3 bench/run-general.py --candidate dist/keel-candidate --profiles \
  --suite memory --out dist/general/heap-by-size --reps 1
python3 scripts/differential.py --bin dist/keel-candidate \
  --redis /path/to/redis-server --out dist/general/differential --steps 30000
python3 scripts/operational-matrix.py --bin dist/keel-candidate \
  --out dist/general/operations
python3 scripts/soak.py --bin dist/keel-candidate --seconds 172800 \
  --cycle-seconds 300 --primary-crash-every 0 --out dist/general/soak-48h
```

`-profile-dir` creates a fresh private directory with CPU, allocation and heap
profiles. `SIGUSR1` captures a live heap/runtime snapshot while clients remain
connected; clean shutdown captures final profiles. It exposes no HTTP endpoint.
Heap captures force GC and profiling allocates its own buffers, so compare them
with other diagnostic captures rather than using their timing or RSS as an
unprofiled performance result. Allocation profiles include preload and warmup.

The differential test compares supported common RESP2 replies and canonical
state against Redis. It normalizes unordered collections and compares errors by
class, not wording. It includes binary strings and integer boundaries; it does
not claim cross-type SET equivalence, identical clocks or all Redis commands.
Every seed and typed command trace is retained for reproduction.

Operational checks cover each eviction policy under key/byte limits, replica
agreement after eviction, two crash/restarts, and independent-client progress
while eight clients do not read their large replies. The soak sustains cache,
hash, list, set and sorted-set writes, 1 MiB values, expiry, rewrite, primary and
replica crashes, stale-read rejection, externally fenced manual promotion and
OS write failures. It checks known acknowledged state after recovery and records
recovery durations. This does not simulate power loss or prove a bound on
asynchronous replication loss.

Long runs append `checkpoints.jsonl` and `recoveries.jsonl`; `progress.json`
contains running status and a bounded recent window, and `report.json` is the
terminal result. A running or interrupted soak is not a pass. The laptop must
remain awake and running for its requested elapsed duration to be exercised.
CPU and latency samples during this diagnostic soak are not matched benchmarks.
With `--primary-crash-every 0`, the primary stays up for the full run, allowing
long-uptime memory growth to be observed while replicas restart. The default
crashes the primary every third recovery cycle and exercises outage handling;
use that separately from the uninterrupted-primary measurement.

The next persistence/replication stages have concrete ownership, visibility,
admission, recovery and testing contracts in
[persistence-replication-next.md](persistence-replication-next.md).
