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
separately. Hosted comparisons below provide candidate evidence; local runs validate the harness only.

General cache validation accepts `rewrite_validation=<exact baseline SHA>` to
run the comparison on a free public Linux runner with separate exposed server
and generator core groups. Public-host tenancy and storage remain uncontrolled.

Eight local one-MiB smoke arms pass: read-only/20% writes, no/always fsync,
both binaries, three completed rewrites per arm and zero protocol errors.
Full combined tests/vet and fault-injection race tests pass. Raw local evidence
is in `bench/results/rewrite-preflush-smoke-2026-09-07.json.gz`.

The first hosted run 34124960981 completed all 36 arms at runtime 4d5b6b0.
Median scheduled p99.9 improved in all six policy/workload combinations and
admission drops fell; everysec with 20% writes regressed at p99 (4.72 to 8.65 ms).
Always with 20% writes was overloaded in both arms, dropping roughly half the
offered work, so its completed-request latency alone cannot describe capacity.
No protocol errors or queue expirations occurred; generator CPU was below
0.076 cores, and workers prepared at least 1,997 ms before the shared start.
Raw evidence remains in `bench/results/rewrite-preflush-first-hosted-2026-09-07.json.gz`.

Subsequent inspection found that the loop would repeatedly wake while waiting
for the replacement sync. The worker now captures a shutdown-safe notification;
the loop self-wakes only while rewrite work can advance. A blocked-sync regression
checks no runnable cycle until completion and exactly one worker notification.
This runtime correction requires its own matched repeat before adoption.

Review hardening checks each binary’s required flags and persistence fields
before the matrix, requires at least two CPUs per disjoint side, and reserves
eight minutes of job headroom for partial artifact upload. The original hosted
run already used two CPUs per side. Independent arm errors are now retained and
remaining arms run before aggregate failure; an injected startup failure produces
four reports (two baseline passes, two candidate failures) and a nonzero exit.
A normal two-arm smoke passes. Cleanup failures from abandoned sync jobs are
logged. Unit compatibility checks, race and workflow lint pass; all attempts
are retained in `bench/results/rewrite-review-guards-2026-09-07.json.gz`.

## Wakeup-corrected matched comparison

Run 34126440085 repeats all 36 arms at e4639168 against the same baseline
66f8ceb3. All 1,080,000 scheduled requests balance and complete, with zero drops,
queue expirations or protocol errors. Generator CPU stays below 0.141 cores.
All 108 rewrites finish inside their measurements, and snapshot contents verify.

| Fsync | Writes | Scheduled p99 ms, before / after | p99.9 ms | Maximum ms across repetitions |
| --- | --- | --- | --- | --- |
| no | 0% | 1.556 / 1.180 | 9.044 / 4.850 | 27.643 / 7.146 |
| no | 20% | 2.458 / 2.081 | 11.534 / 4.588 | 21.397 / 12.063 |
| everysec | 0% | 1.901 / 1.196 | 12.714 / 5.177 | 28.617 / 8.020 |
| everysec | 20% | 2.425 / 2.130 | 11.272 / 4.850 | 17.106 / 12.571 |
| always | 0% | 1.425 / 1.180 | 11.928 / 4.030 | 20.311 / 6.417 |
| always | 20% | 4.096 / 3.310 | 14.287 / 12.845 | 28.262 / 19.138 |

Both arms ran much faster than on the first runner. Therefore only within-run
comparisons support the candidate’s latency benefit; the between-run improvement
cannot be attributed to the wakeup correction. In this repeat, candidate p99
and p99.9 improve in every tested combination. Rewrite completion medians remain
roughly 47–56 ms; background preflush improves serving latency rather than
promising faster rewrite completion. Public storage variability and the first
run’s mixed p99/overload results remain explicit. The paired evidence supports
adoption, without a fixed latency or data-loss guarantee. Raw evidence is in
`bench/results/rewrite-preflush-wakeup-hosted-2026-09-07.json.gz`.

The branch also integrates the separately validated opaque-record change. Its
20 mutation cases, existing rewrite/replication tests, focused race and full
combined suite pass together. Later harness review changes leave the measured
normal-path runtime unchanged.

The final lifecycle review reproduced an existing wakeup callback/descriptor
teardown race. Detaching the waker now waits for an active callback to finish
before descriptors can close or be reused. The baseline fails a blocked-callback
regression; the candidate passes focused shutdown/wakeup/pipeline race checks,
full tests and vet. Raw evidence is in `bench/results/waker-lifecycle-2026-09-07.json.gz`.
The callback must not register another waker. Integration now includes merged
client fairness as well as opaque records; final matched checks use that same
combined baseline on both sides.

## Combined candidate and original-log sync diagnostic

Run 34128657503 compares af1ae853 against the merged fairness/opaque baseline.
Ordinary matched median ratios are 1.003 (small read), 1.010 (many clients),
1.017 (pipeline 16) and 1.020 (pipeline 64), with no generator CPU warnings.
All 36 rewrite arms and 108 rewrites complete; all 1,080,000 scheduled requests
complete with no drops, expiry or protocol errors. This is paired public-runner
evidence, not a dedicated-host capacity claim.

Combined archive validation run 34128192062 timed out on Linux inside the
latency diagnostic's quadratic insertion sort, after its rewrite loop finished.
The diagnostic could collect idle polls while waiting for the original log's
background sync. Sorting now uses the standard O(n log n) implementation, and
worker waits are measured separately from event-loop work. A local million-key
repeat records 977 work slices and finishes in 0.88 seconds; this is diagnostic
evidence, not an end-to-end latency guarantee.

A blocked-original-sync regression also reproduces unnecessary runnable rewrite
cycles on the preceding candidate. The server now waits for that sync's explicit
completion notification before finalization; ordinary command traffic continues.
The previous candidate fails the regression; the corrected candidate passes three
race repetitions, full tests and vet. A final matched repeat and archive validation
are required for this runtime change. Raw hosted results, the original timeout
stack and both regression outcomes are retained in
`bench/results/rewrite-integrated-sync-diagnostic-2026-09-07.json.gz`.
