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

This measures live Go heap, not a process RSS decrease, a throughput improvement
or a deployment capacity limit. Partly occupied expiry tables, lookup-map
capacity and sparsely occupied key pages can still retain memory. Incremental
shrinking of those nonempty structures and larger churn/connection sweeps remain
separate follow-ups. The active long soaks use older frozen binaries.
