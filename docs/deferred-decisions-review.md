# Review of PR 33's deferred decisions

Reviewed 6e0fcd1b8f78f19ba2e42e18bfe3b5cb76a71a4c on September 7, 2026.
Keeping the existing eviction barrier while measuring alternatives is reasonable.
The proposed documents need corrections before they describe current behavior
or justify rejecting future work:

- Rewrite slicing does not mean rewrites cannot stall command execution.
  Opaque structure serialization, file writes and final fsync remain synchronous.
  The merged traversal uses bounded stable slots, not whole-shard name batches.
  PR 28 separately adds bounded large-record streaming.
- Dirty reconciliation cannot make reading a concurrently mutated Go value safe.
  It can repair a coherent stale snapshot; it cannot repair a data race, torn
  string/slice header, invalid pointer, panic or malformed intermediate record.
  An ownership/snapshot protocol is required before handing state to a worker.
- Serving while an append runs is conditional: admission, queue budgets and
  unmodelled commands can require the barrier. Replies are gated by the configured
  persistence boundary, which is not a durable-on-disk prefix under every policy.
  The `no` and `everysec` policies do not inherit `always` durability.
- A cache operating at maxmemory is a normal workload. No profile in this proposal
  establishes that eviction dominates its append barrier or that removing the
  barrier would yield only a small gain. The proposed pre-eviction section also
  claims a conservative overestimate can be insufficient without identifying
  an unmodelled source of growth.
- The quoted 20 ns lock and 110 ns dispatch timings are not a matched client
  throughput measurement or evidence of a 20% end-to-end regression. Different
  ownership strategies need measured costs and correctness proofs.

The practical next step remains controlled profiles and matched workloads under
identical persistence/eviction settings. This review does not approve PR 33's
current claims or implement parallel command execution. PR 31's separate restart
guard defect is recorded in `docs/failover-31-review.md`.
