# Engineering closeout

Started September 7, 2026. Work is integrated separately from the active append
admission branch and the frozen long-soak binaries. GoGIF remains unchanged.
Spending remains capped at zero.

| Finding | Change | Validation state |
| --- | --- | --- |
| Unsafe failover proposal | Verified external fencing precedes authenticated incarnation-specific activation; local terms are not fencing | Finite model and counterexample tests pass; real provider implementation remains future work |
| Unsupported Go builder | Source/CI floor 1.26; Docker builder 1.27.1 | Local compiler/build checks running; native CI/container validation pending |
| SCAN compatibility | Option-presence handling, case-insensitive TYPE, explicit regression and differential cases | Focused command tests pass; differential integration pending |
| Traversal and adoption gate | Stable paged slots, strict scan work limit, bounded pattern work, shared rewrite/snapshot name batches | Traversal, heap-accounting and existing core tests pass; complete correctness and matched Linux matrix pending |

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
