# v0.1.0-alpha.3 candidate

This increment restores production active expiry, moves everysec fsync off the
command thread, and yields between chunks of large lists during AOF rewriting.

Two optional experiments are added:

- `-aof-async-append`: one worker batch, command backpressure and a persistence reply
  barrier. It does not execute commands concurrently with unacknowledged writes.
- `-replication-feed` / `-replicaof`: authenticated canonical state replication,
  read-only/stale-read gates, epoch/offset/checksum validation, and manual promotion
  after external fencing. The initial dataset/frame limit is 8 MiB; retained history
  is bounded to 16 MiB / 1024 batches. This is asynchronous replication with possible
  data loss on primary failure, not automatic failover or a zero-loss protocol.

Archives include runnable Bencher, k6 and AWS DLT adapters. Their local/native CI
smokes validate integration; paid provider runs and real application pilots remain
pending account/target setup. Existing latency smoke results establish no speedup.
See [replication-alpha.md](replication-alpha.md) and the README for exact limits.

The [candidate validation record](pr15-validation-2026-09-05.md) tracks the Linux
race-job correction, benchmark review, native checks and archive verification.
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
