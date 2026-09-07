# Empty expiry-table retention

The September 7 heap test reproduced about 3,495,040 bytes retained after
removing every TTL from a 100,000-entry expiry table. This happened for strings
and typed collections, through PERSIST, overwrite and deletion. Go's map kept
its capacity after its last entry was removed.

Track the peak entry count of each expiry table and release an empty table once
it has held at least 256 entries. Smaller tables remain reusable: discarding
them after every single-key SET/TTL update would introduce repeated allocations
on a common cache workload. TTL-free stores create their table lazily.

All six full-GC heap regression cases now return within 512 KiB of the pre-TTL
baseline; this local run returned within a few KiB. The single-key churn checks
perform zero allocations per clear/reapply cycle. Existing keyspace accounting,
expiry, persistence and the complete local Go suite pass. Raw before/after output
is retained in `bench/results/ttl-empty-retention-2026-09-07.txt`.

Hosted validation for c7125f5 passed the
[Go/race/Docker matrix](https://github.com/brandopakel/keel/actions/runs/34099308703),
[Redis differential, workloads and operational checks](https://github.com/brandopakel/keel/actions/runs/34099310984),
and [native ARM64/Intel Mac plus ext4/XFS recovery](https://github.com/brandopakel/keel/actions/runs/34099313025).
Integration with the merged traversal closeout receives the same checks again.

The first integrated ARM64 run
([34101698050](https://github.com/brandopakel/keel/actions/runs/34101698050))
failed its short soak after 23,437 acknowledged writes with a three-second SET
timeout. Its last completed checkpoint showed no pending append or write error;
the existing logs do not establish the cause. This failure remains evidence,
not a passed run. The harness now records fresh-connection INFO and Go goroutine
stacks before shutting down an unexpectedly failed owned process. Three fresh
five-minute ARM64 repetitions with unchanged client deadlines are pending.

This measures live Go heap, not a process RSS decrease, a throughput improvement
or a deployment capacity limit. Partly occupied expiry tables, lookup-map
capacity and sparsely occupied key pages can still retain memory. Incremental
shrinking of those nonempty structures and larger churn/connection sweeps remain
separate follow-ups. The active long soaks use older frozen binaries.
