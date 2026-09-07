# Rewrite preflush candidate — 2026-09-07

The replacement-file fsync previously ran entirely on the command loop after
the incremental snapshot walk. This candidate performs one bulk preflush on a
worker that owns only the replacement file. Commands and normal AOF appends
continue; changed key names remain tracked by the existing bounded dirty set.
After the worker finishes, the loop emits the dirty tail and synchronizes it
before the existing atomic replacement. Repeated asynchronous resync was rejected
because continuous writes could prevent convergence until the duration abort.

Only one replacement-sync worker/file may exist. Cancellation transfers cleanup
to that worker and prevents reuse of the temporary pathname until it finishes.
Graceful AOF close joins it. Sync errors and rewrite budget aborts preserve the
original log. Fault tests cover blocked sync with writes, cancellation, both
budgets, initial sync failure and final dirty-tail sync failure; acknowledged
values replay from the original log after failure. Existing bounded-slice tests
wait for worker completion separately so scheduler polling does not count as
emitted work. `INFO persistence` exposes `aof_rewrite_pending_sync` until the
event loop consumes the worker completion, including cancellation cleanup.

This does not make all persistence asynchronous: replacement writes, opaque
serialization, the final dirty-tail sync, rename and directory sync can still
stall command execution. One atomic command can also exceed cooperative targets.
There is no fixed latency or durability improvement claim.

`bench/run-rewrite-validation.py` compares fresh processes with identical append
settings, rotated arm order, a 32 MiB snapshot dataset and independent Go
scheduled-arrival traffic at 2,000 commands/s. Three rewrites must finish inside
each 15-second measurement. Read-only and 20% writes are repeated three times
under no/everysec/always fsync, for 36 arms. The bounded queue reports drops,
expiry, errors, scheduled/service latency and generator CPU. INFO polling every
10 ms and ps sampling every 250 ms are identical in both arms. Full snapshot
contents are verified after measurement; crash/restart correctness is covered
separately. Hosted evidence is pending; local runs validate the harness only.

General cache validation accepts `rewrite_validation=<exact baseline SHA>` to
run the comparison on a free public Linux runner with separate exposed server
and generator core groups. Public-host tenancy and storage remain uncontrolled.

Eight local one-MiB smoke arms pass: read-only/20% writes, no/always fsync,
both binaries, three completed rewrites per arm and zero protocol errors.
Full combined tests/vet and fault-injection race tests pass. Raw local evidence
is in `bench/results/rewrite-preflush-smoke-2026-09-07.json.gz`.
