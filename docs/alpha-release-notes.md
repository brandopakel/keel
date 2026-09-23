# v0.1.0-alpha.4

This increment brings the development work merged since alpha.3 to a published
build: typed string storage, stable paged traversal and `SCAN`, streamed large
collection records, additional sorted-set operations, TTL/lookup/set-map
compaction, client fairness, reply and request allocation admission, and a
Linux readiness optimization that skips unchanged `epoll_ctl` registrations.

Two experiments are extended and remain opt-in:

- `-aof-concurrent-append` (requires `-aof-async-append`): bounded string
  commands run while earlier writes are still being appended, with replies
  released in log order.
- `-replication-protocol 2`: streaming snapshots, operation deltas, replica
  recovery checkpoints and a durable failover term (`KEEL.PROMOTE` /
  `KEEL.FENCE`). Replication is still asynchronous and can lose acknowledged
  writes on primary failure. Automatic failover is not implemented, and manual
  promotion still requires external fencing. See
  [replication-v2.md](replication-v2.md) and [failover-design.md](failover-design.md).

The event loop now closes a connection it has stopped serving instead of
leaving the client waiting, and it logs why. There are three cases: a request
parsed and not answered within 30 seconds, request bytes left unread, and a
run that produces no reply. `INFO clients` reports each as
`clients_closed_unanswered`, `clients_closed_unread` and
`clients_closed_unreplied`. All three should stay at zero, and a nonzero count
is a bug worth reporting.

**Replies wait for the disk under `appendfsync always`.** A write is
acknowledged only after its fsync, so a disk that stalls for ten seconds delays
that write's reply by ten seconds. Other clients keep being served in the
meantime when `-aof-concurrent-append` is on. This is the durability contract
working as designed, and Redis behaves the same way. On GitHub-hosted runners
fsyncs of 4 to 13 seconds were measured, so set client timeouts with the disk
in mind, or use `appendfsync everysec` where losing up to a second of writes
is acceptable.

Validation for this build:

- The protocol 2 concurrent continuous-primary shape ran 48 hours as one
  primary process twice on a small Always Free x86 VM, with no growth breach, no
  unanswered closure and flat RSS (runs 35352473161 and 35502854458). The
  48-hour run on this tag's commit is linked from the release.
- A nightly three-arm soak runs on GitHub-hosted runners. The intermittent
  "liveness stall" it reported in September 2026 was traced to hosted-runner
  fsync latency exceeding the harness's three-second client timeout, not to a
  server defect. See [long-run-gate.md](long-run-gate.md).
- The release workflow upgrades from the published alpha.3 archives on all
  four native targets, rewrites, restarts and rolls back from backup.

Persistence written by alpha.3 is read by this version. Back up persistence
files before upgrading. This remains an alpha: see the README for the
integration contract and the boundaries that stay.

---

The following notes describe the previously published alpha.3 and are historical.

# v0.1.0-alpha.3

This increment restores production active expiry, moves everysec fsync off the
command thread, and yields between chunks of large lists during AOF rewriting.
After snapshot traversal, reconciliation waits for in-flight fsync so hot keys are
not repeatedly emitted while file replacement is blocked. Failed socket readiness
registration releases connection accounting, and
failed replication application disables core reads until a full synchronization.

Two optional experiments are added:

- `-aof-async-append`: one worker batch, command backpressure and a persistence reply
  barrier. It does not execute commands concurrently with unacknowledged writes.
- `-replication-feed` / `-replicaof`: authenticated canonical state replication,
  read-only/stale-read gates, epoch/offset/checksum validation, and manual promotion
  after external fencing. The initial dataset/frame limit is 8 MiB; retained history
  is bounded to 16 MiB / 1024 batches. This is asynchronous replication with possible
  data loss on primary failure, not automatic failover or a zero-loss protocol.

Archives include runnable Bencher, k6 and AWS DLT adapters. Their local/native CI
smokes validate integration, and matched GitHub measurements are published to the
Keel Bencher project. Paid provider execution and real application pilots remain
pending host/workload setup. Matched results establish no worker-append speedup.
See [replication-alpha.md](replication-alpha.md) and the README for exact limits.

The [candidate validation record](pr15-validation-2026-09-05.md) tracks the Linux
race-job correction, benchmark review, native checks and archive verification.
The [release closeout](alpha3-closeout.md) adds native archive installation,
24 alpha.2 upgrade/rollback cases across all four native targets, matched benchmarks
and a 15-minute operational soak with crash recovery and disk-full tests.
Tag-built artifacts require their own checksum verification before publication.

---

The following notes describe the previously published alpha.2 and are historical.

# v0.1.0-alpha.2

Keel now offers cross-type expiry, conditional SET options, complete per-key memory reporting,
and a tested authenticated cache/analytics integration. Recovery and socket handling have been
hardened against the process crashes, replay corruption, false durability acknowledgments, and
slow-reader stalls reproduced in the engineering review.

Behavior changes:

- Standalone binding defaults to localhost; container custom arguments must retain `-host 0.0.0.0`.
- AOF is accepted only with the production event-loop mode.
- Oversized/stalled clients can be disconnected. An interrupted write has an uncertain outcome.
- New dumps have a KEL1 version prefix and CRC32 checksum. Old readers cannot consume new dumps;
  this version still reads old payloads. Back up persistence files before upgrading.
- A torn tail is preserved and repaired before service resumes. Other replay errors prevent startup.
- Rewrite resource limits can decline or abandon a rewrite while leaving the original log intact.

This remains an alpha. Native TLS, ACL roles, transactions, replication, cluster routing and an
embedding API are absent. Synchronous persistence, individual large keys and expensive commands
can still cause latency spikes. See the README for the complete integration contract.

Local candidate archives carry a `-dev` suffix until built from a published release revision.

The alpha.1 candidate was withheld after the release latency gate found that a
2 ms CPU slice left no room for its filesystem write. Alpha.2 uses a 1 ms CPU
slice while retaining the existing 2 ms median-slice validation threshold.
