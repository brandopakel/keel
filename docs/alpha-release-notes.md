# v0.1.0-alpha.1 candidate

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
