# Rewrite resource limits

Follow-up to the traversal candidate, September 7, 2026. This branch is separate
from the frozen traversal benchmark and existing long-soak binaries.

Large string values, collection members and their key names are emitted as RESP
fragments with at most 64 KiB added per stream advance. The stream holds immutable
string references instead of constructing a second complete encoded value.
Normal small records retain the batched path. Existing collection cursors still
bound each logical collection batch to 256 members and a 64 KiB target.

A key can change or disappear while its record is partly written. That record
must finish before any subsequent command is emitted; otherwise the rewrite
would contain malformed RESP. Dirty reconciliation then replaces its old state.
Cancelling a rewrite discards the temporary file and retained stream; the original
append-only file remains authoritative. No new persistence format is introduced.

Dirty tracking admits a new name only while both the 100,000-name limit and an
8 MiB budget (name bytes plus 64 bytes per entry) fit. Repeated changes to the
same name do not repeatedly consume the budget. Exceeding a limit abandons the
rewrite while writes continue into the original log. INFO persistence exposes
`aof_rewrite_dirty_keys`, `aof_rewrite_dirty_bytes` and
`aof_rewrite_budget_aborts`; these are bookkeeping budgets, not process RSS.

Tests cover oversized strings, hashes, lists, sets and sorted sets, each unchanged,
replaced or deleted during a partial record, including TTL preservation/reset and
replay. Separate tests verify that starting a 4 MiB string record allocates less
than 256 KiB, cancellation preserves the original log, and a dirty-byte refusal
keeps later writes recoverable. Full local tests and vet passed, followed by the additional cancellation and
accounting regression checks. Hosted validation of `ff0b6aa` passed: Go 1.26/stable Linux/macOS, race, Docker,
Redis differential, 24 workload smoke cases, native ARM64/Intel Mac recovery,
ext4/XFS and ENOSPC. The raw CI runs and compact reports are recorded in
`bench/results/rewrite-resource-validation-2026-09-07.json`.

Both hosted combined-fault modes completed ten rewrites during 2,500 writes
while two replicas were offline, expired all 64 short-TTL fixtures and recovered
the surviving acknowledged values on both replicas and after primary restart.
Their observed maximum write times were 7.43 and 12.58 ms on that runner; these
short checks are correctness evidence and do not establish a latency SLO.

Opaque probabilistic structure serialization, file writes and final fsync still
have synchronous work. This change does not establish a latency SLO, remove all
rewrite stalls, or raise the current rewrite duration/key-count limits. Retaining
an old immutable value during replacement still requires transient memory.

Reply frame indices used by unbuffered writes are also charged to retained reply
memory. Charging them to input left the reply-class admission limit understated;
the regression test checks both the class boundary and repeated accounting.
# Integrated validation

After integration with merged PR 27, commit 70f253a passed the
[Go/race/Docker matrix](https://github.com/brandopakel/keel/actions/runs/34101668336),
[Redis differential, workload and combined operational checks](https://github.com/brandopakel/keel/actions/runs/34101668312),
and [native ARM64/Intel Mac plus ext4/XFS recovery](https://github.com/brandopakel/keel/actions/runs/34101668332).

The earlier frozen runtime also completed its separate four-hour ext4 and XFS
recovery soaks in [34083541166](https://github.com/brandopakel/keel/actions/runs/34083541166).
Each completed 15 primary and 32 replica crash recoveries. Those runs exercise
the pre-closeout runtime and do not validate the new rewrite implementation.
Their differing write counts are not a controlled filesystem performance
comparison. The Mac eight-hour and 48-hour soaks are still running separately.
