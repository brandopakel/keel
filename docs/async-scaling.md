# Asynchronous I/O, latency evidence and scaling

Status: September 5, 2026 working-tree implementation and proposed next contracts.
The published alpha.2 does not include these changes.

## Implemented

Everysec fsync executes in one background worker with no unbounded work queue.
The event loop owns all persistence state; the worker captures its descriptor and
sync function, then reports one result through a buffered channel. Writes made
while syncing remain dirty for the next sync. Appends still precede replies.
Errors become sticky when observed before a subsequent reply batch. Previously
acknowledged everysec writes were never guaranteed durable. Shutdown joins the
worker before closing its descriptor; rewrite finalization yields until it finishes.
Always continues to sync before replies. This does not make appends nonblocking:
filesystem writes can stall, including while fsync is running.

Large lists are rewritten in chunks of 256 elements, targeting 64 KiB per chunk.
Any mutation, deletion, expiry or type replacement invalidates the cursor. Dirty
reconciliation emits DEL and starts again, so partial historical fragments cannot
become the final state. A single large element can exceed the byte target. Large
hashes, sets, sketches, snapshot enumeration and whole-key responses remain work.

## Benchmark contract

`bench/run-tail.py` launches a fresh owned process per repetition and persistence
policy. It warms a cache, builds a large list, sends mixed GET/SET-with-TTL/LRANGE
traffic, and requests a rewrite during persistent runs. A separate connection
sends scheduled probes at 100 Hz. CSV records service time and time from scheduled
arrival, including probe scheduling delay; errors are retained and fail the run.
Metadata identifies the executable by SHA-256. Client and server share a host;
Python scheduling/GIL and response decoding affect measurements. Load traffic is
closed-loop: it cannot establish an open-loop capacity curve. Five-second local
runs are smoke evidence; p99.9 with 500 probes is particularly under-sampled.

Before making performance claims, run:

| Dimension | Required coverage |
| --- | --- |
| Offered load | Open-loop rate sweep below/near/above capacity; bounded queues and reported drops |
| Client placement | Separate load host; loopback and deployment network, pinned tool versions |
| Data | Strings, hashes/lists, probabilistic analytics; realistic size/skew/TTL distributions |
| Concurrency | 1/16/100/1000 clients, pipeline 1/16/64, slow readers and reconnect storms |
| Persistence | Off/no/everysec/always, rewrite under sustained mutation, slow/full/failing disk |
| Duration | Warmup, at least five independent repetitions, 30-minute soak and overnight soak |
| Outcomes | Throughput, p50/p95/p99/p99.9/max, errors/timeouts, RSS/CPU/GC, expiry lag, recovery time |
| Comparisons | Exact binaries/configuration, matched durability and resource budgets, raw artifacts |

The Linux workflow runs a 30-second/repetition diagnostic; it is not the full matrix
above. Agree application acceptance thresholds before running pilots.

## Asynchronous append boundary — first implementation available

The optional one-batch worker/barrier implementation is described in
[replication-alpha.md](replication-alpha.md). It pauses command execution while
appending and keeps replies behind the persistence barrier. A future concurrently
executing append path needs an ordered, byte-bounded queue and log
offsets. Reply batches must wait for their required append/durability offset while
the event loop continues serving sockets. Queue saturation needs admission control;
a disk error must discard unacknowledged success batches. Rewrite cutover and
shutdown require fences against the same offset sequence. Reads of unacknowledged
mutations need an explicit visibility contract. Merely acknowledging a queued
write would weaken today's contract and is not an acceptable implementation.

## First replication contract — bounded experimental implementation

One primary and a read-only replica with manual promotion are now implemented
behind explicit flags, with an 8 MiB dataset/frame limit. See the
[implementation and failure contract](replication-alpha.md). Adoption still requires
the pilot to accept asynchronous loss and manual external fencing.

Implemented first-stage protocol elements: primary identity/epoch and monotonic offsets; authenticated
versioned transport; consistent full synchronization plus a bounded catch-up log;
checksummed frames; disconnect/resync on lag overflow; ordered application of
canonical mutations (including random outcomes, expiry and eviction); read-only
command enforcement; lag/offset diagnostics; and local AOF persistence. Replica restart currently
requires full synchronization rather than a durable resumable checkpoint. Manual
promotion requires the operator to fence the old writer; the server does not
provide distributed fencing or an election. A replica must not independently evict
keys or draw different random outcomes. Filesystem AOF positions alone are not
stable replication offsets because rewriting changes them.

Acceptance tests must include full sync during mutation, reconnect with/without
retained history, slow replica, corrupt frame, primary crash, replica restart,
expired keys, every supported data type, and old-primary reappearance. Measure
loss and recovery time. Automatic failover requires election/quorum and fencing;
zero acknowledged-write loss requires a stronger acknowledgement and durability
protocol. Neither follows automatically from adding a second copy.

Redis likewise distinguishes asynchronous replication from strong consistency:
[replication documentation](https://redis.io/docs/latest/operate/oss_and_stack/management/replication/)
and [WAIT semantics](https://redis.io/docs/latest/commands/wait/).
Its [latency documentation](https://redis.io/docs/latest/operate/oss_and_stack/management/optimization/latency/)
also explains why background fsync alone does not eliminate disk-write stalls.

## External evidence still needed

The owner must identify an application and its data-loss, recovery, memory and
latency requirements. No outside users have been contacted and no real pilot has
been claimed. Run the existing cache/analytics integration against that workload,
record unsupported commands and installation effort, and compare the incumbent.
Choose embedding only for measured service-boundary cost, partitioning for capacity,
and replication for failure recovery. These remain separate commitments.

## External testing and benchmarking decision

Recommendation: use external infrastructure for independent performance evidence,
while retaining repository-owned tests and portable workload scripts. A hosted
service should orchestrate and visualize measurements; it should not become the
sole correctness oracle or the only place a workload can run.

| Layer | Execution location | Purpose |
| --- | --- | --- |
| Every change | Existing Linux/macOS CI | Unit/integration, race, fuzz smoke, persistence and failure regressions |
| Nightly | Controlled dedicated runner, separate load host | Matched baseline/candidate rate sweeps and longer soaks |
| Release candidate | Repeatable external deployment matching the pilot | Network, storage, resource budgets, recovery and sustained-load evidence |
| Application pilot | Owner-approved real application environment | Useful command compatibility, installation effort, application SLO and memory advantage |

Start with two controlled cloud hosts and the existing RESP tools. Fix instance
shape, OS/kernel, CPU allocation, storage type, placement and network; record them
with binary hashes and configuration. Randomize/interleave baseline and candidate
order and repeat runs. Shared hosted CI is useful for coarse regressions but is
not our basis for small percentage performance claims. External infrastructure
alone does not make a benchmark independent of its workload assumptions.

Service candidates, assessed against their official documentation:

- **Controlled hosts running memtier and our harnesses:** initial preference for
  direct RESP coverage and reproducible artifacts. Memtier supports Redis traffic,
  custom commands and TLS; our Keel-specific replay/error checks remain necessary.
  [Official memtier repository](https://github.com/redis/memtier_benchmark).
- **Grafana k6/Cloud:** evaluate if managed orchestration and shared dashboards
  justify the integration. k6 lists a TCP extension, but generic TCP support does
  not supply a Keel-aware RESP workload or prove that the required extension is
  supported by a particular hosted execution mode. Validate framing, authentication,
  custom commands, connection reuse, scheduled arrivals and raw export in a small
  proof of concept before selecting it.
  [Official extension catalog](https://grafana.com/docs/k6/latest/extensions/explore/) and
  [Cloud-supported extensions](https://grafana.com/docs/grafana-cloud/testing/k6/author-run/use-k6-extensions/).
- **Bencher:** evaluate for continuous benchmark history and controlled runner
  execution alongside our load scripts. Its self-hosted bare-metal runner model
  is relevant to repeatability; it does not replace application load generation
  or persistence correctness assertions.
  [Official runner documentation](https://bencher.dev/docs/explanation/self-hosted-runners/).
- **Distributed Load Testing on AWS:** evaluate if AWS is the pilot's deployment
  environment. The solution orchestrates JMeter, k6 and Locust scripts. Verify
  custom TCP/RESP packaging, private connectivity, generator saturation and raw
  results rather than assuming an HTTP example covers this server.
  [Official solution overview](https://docs.aws.amazon.com/solutions/latest/distributed-load-testing-on-aws/solution-overview.html).

Selection gates: RESP2 and custom-command support; private authenticated access;
separate client/server telemetry; scheduled-load accounting including dropped
work; raw histograms/errors and export; pinned generator versions; reproducible
infrastructure; workload portability; generator CPU/network headroom; and a defined
cost ceiling. A managed dashboard is optional; these properties are mandatory.

Concrete next experiment: compare one controlled-host run with one candidate
service using identical small-key, large-list and rewrite workloads. Reconcile
request counts, errors and latency distributions. Adopt the service only if it
reduces maintenance without obscuring results. Keep fault injection and replay
in CI even if load generation moves to a provider. No paid infrastructure or
external testing account has been provisioned in this change.

Runnable provider adapters and their actual local validation are documented in
[bench/external/README.md](../bench/external/README.md). Hosted execution remains pending account/target setup.
