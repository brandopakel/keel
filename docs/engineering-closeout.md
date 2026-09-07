# Engineering closeout

Started September 7, 2026. Work is integrated separately from the active append
admission branch and the frozen long-soak binaries. GoGIF remains unchanged.
Spending remains capped at zero.

| Finding | Change | Validation state |
| --- | --- | --- |
| Unsafe failover proposal | Verified external fencing precedes authenticated incarnation-specific activation; local terms are not fencing | Finite model and counterexample tests pass; real provider implementation remains future work |
| Unsupported Go builder | Source/CI floor 1.26; Docker builder 1.27.1 | Full local tests/vet and Linux/macOS/native/container CI passed |
| SCAN compatibility | Option-presence handling, case-insensitive TYPE, explicit regression and differential cases | Focused command tests pass; Linux differential integration passed for the first traversal candidate |
| Traversal and adoption gate | Stable paged slots, strict scan work limit, bounded pattern work, shared rewrite/snapshot name batches | Traversal, heap-accounting and existing core tests pass; full local Go tests passed; both matched Linux matrices completed; longer targeted run pending |

A design fix is distinct from implementing automatic failover. Cooperative
scheduling is distinct from a real-time latency guarantee. The new traversal
still needs the complete memory/throughput evidence before an adoption decision.

Subsequent work remains tracked until validated: dirty/rewrite byte limits and
large-value handling; retained memory under churn; fair per-client admission;
append pipeline diagnosis and extended replication catch-up; broader clients,
telemetry and combined operational modes; offered-rate/capacity sweeps and
representative application traces. Separate architectural commitments include
embedding, partitioning, transactions and automated high availability.

Running soaks count only when their terminal reports complete. They cover their
recorded frozen source and binary hashes, not arbitrary later changes. Final
release validation must use the actual combined revision and downloadable
artifacts. No release has been made by this closeout work.

The branch now integrates develop through PR #26, including collection admission
and separate retained-input/reply budgets from PR #25. Four newly reproduced
admission underestimates were corrected: repeated HMGET fields, wide SMISMEMBER
replies, new sorted-set metadata and list ring growth. Their regression tests and
the full admission suite pass. These changes require validation of the integrated
revision in addition to the frozen first traversal matrix.


## Review evidence

The completed Go/container jobs for `cd2b740` are recorded in
[Go CI](https://github.com/brandopakel/keel/actions/runs/34096730768); native
ARM64/Intel Mac and ext4/XFS checks are in
[platform CI](https://github.com/brandopakel/keel/actions/runs/34096730684).
The earlier PR body still said pending and has been corrected. Completed checks
remain tied to their revision; later changes receive their own validation.

The review's suggested removal of mixed-case SCAN TYPE differential checks was
rejected: Redis's `getObjectTypeByName` uses `strcasecmp` in
[Redis 8.2 source](https://github.com/redis/redis/blob/8.2/src/db.c#L1309), and the
actual Redis differential runs pass those cases. Empty and mixed-case options
remain explicit compatibility regressions.

Character-class ranges and escapes now charge all consumed bytes, and the unused
class wrapper is removed. The native load generator chooses the largest divisor
of the client count within its thread maximum (six requested threads and sixteen
clients selects four). Existing comparisons used a maximum of two, for which the
old and new selection agree. The failover model deliberately retains delayed
local activation as an adversarial case; its safety invariant is externally
unfenced writers, not instantly synchronized local role flags.
