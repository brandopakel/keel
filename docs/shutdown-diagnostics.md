# Shutdown grace and large-write diagnostics

Keel defaults to a five-second grace period after SIGTERM/SIGINT. The
`-shutdown-timeout 30s` option allows a deployment to choose a longer positive
period for server cleanup and the final persistence sync. A second termination
signal still exits early. Timeout logs include a bounded 1 MiB goroutine dump;
cleanup and persistence errors remain failures. This changes the permitted wait,
not the fsync policy, acknowledgment guarantee or filesystem speed. Operating
system calls blocked in the kernel can delay process exit beyond that period.

## Observed failure

[Hosted run 34168350873](https://github.com/brandopakel/keel/actions/runs/34168350873)
used four clients writing 1 MiB values to four keys for ten seconds, with automatic
rewrite disabled. This is a shutdown reproducer, not a capacity benchmark. Exact
merge source and binary build metadata accompany every arm in
`bench/results/shutdown-five-second-diagnostic-2026-09-07.tar.gz` (nested ZIPs keep
failure persistence compressed; do not expand those AOFs on a development laptop).

| Policy / append mode | AOF GiB | Shutdown seconds | Result |
| --- | ---: | ---: | --- |
| no / sync | 3.63 | 13.634 | failed |
| no / barrier | 4.77 | 5.577 | failed |
| no / concurrent | 5.05 | 5.778 | failed |
| everysec / sync | 5.18 | 4.924 | passed |
| everysec / barrier | 4.63 | 3.271 | passed |
| everysec / concurrent | 3.31 | 6.127 | failed |

Each failed arm captured a final `fsync` blocking cleanup, either directly in
`CloseAOF` or through its pending sync worker. All six write bursts completed with
no reported workload errors before shutdown. The failures remain recorded. This
explains these reproductions; the older stalled Mac soak still lacks a root cause.

## Validation and limits

Focused tests cover cleanup success/error propagation, a second termination
signal and the configured deadline with a bounded stack report. They passed locally
in a guarded 6.26-second invocation; compilation caches were pruned. Evidence is
`bench/results/shutdown-configured-deadline-2026-09-07.json.gz`.

The six-arm [hosted repeat 34170236418](https://github.com/brandopakel/keel/actions/runs/34170236418)
passes with an explicit 30-second grace on candidate `e6b82d9`. Shutdown durations
range from 4.475 to 11.389 seconds. Full reports and build identities are in
`bench/results/shutdown-thirty-second-diagnostic-2026-09-07.tar.gz`.
These runner samples do not establish a universal sufficient grace.
Comparative throughput tests must give both runtime versions the same supported
grace, workload and durability settings. A longer grace does not establish a
persistence speedup or guarantee recovery from storage failure.

Review extends the diagnostic beyond live-memory checks: each writer now stores a
unique sequence, and a fresh process must recover every final acknowledged value
from the closed AOF before the run passes. The pre-replay AOF is preserved in a
compressed failure archive because replay can repair a torn tail. Successful raw
AOFs and that temporary backup are pruned after reports are written. This stronger
recovery gate passes all six arms in [run 34170889997](https://github.com/brandopakel/keel/actions/runs/34170889997)
on `91ed3b0`. Replay readiness ranged from 0.814 to 1.098 seconds; the exact
final acknowledged sequences matched in every arm. Reports and build identities
are in `bench/results/shutdown-replay-diagnostic-2026-09-07.tar.gz`.
The prior six passes covered shutdown alone.
The workflow now runs for relevant runtime, persistence and helper changes.

Pass-result gates and the server readiness probe use explicit checks, so optimized
Python cannot remove writes or mismatch detection. Three tests run the actual
diagnostic with fake server I/O under `python -O`: successful writes/recovery,
invalid acknowledgments and a stale replay value. They passed locally in 0.51
seconds and preserve the original AOF on the injected replay failure. Evidence is
`bench/results/shutdown-optimized-python-gates-2026-09-07.json.gz`.
