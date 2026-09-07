# Socket readiness registration

The separate Linux diagnostic profiles from run
[34097519300](https://github.com/brandopakel/keel/actions/runs/34097519300)
attribute 83% of CPU samples to syscalls in the 100k-key workload;
`Epoll.Monitor` accounts for 8.9% cumulatively. The profile was collected
separately from the throughput comparison and does not itself prove an
optimization will improve throughput.

The event loop registered read readiness after every fully flushed reply,
including connections already registered for reads. Both Linux epoll and
Darwin kqueue use level-triggered registrations here: unchanged interest
does not need rearming. Remember the successfully registered operation on
each client and invoke the multiplexer only when it changes. Keep read,
write and append-pause transitions; failed changes are not cached. A new
connection has independent state even when its descriptor number is reused.

The full local Go suite and vet pass, including server and process integration
checks. Hosted [Go/race/Docker](https://github.com/brandopakel/keel/actions/runs/34100936511),
[native ARM64/Intel Mac and filesystem recovery](https://github.com/brandopakel/keel/actions/runs/34100939434),
and [differential/workload/operational checks](https://github.com/brandopakel/keel/actions/runs/34100933346)
passed. These exercise blocked writes, append reply gating, disconnects and
descriptor reuse.

The same run compared 6fbeacc with c9e84c0 across 24 workloads and nine memory
cases, three repetitions each, on one standard Linux VM with disjoint exposed
physical-core groups. Go 1.27.1 and native memtier 2.5.1 were pinned; AOF was off,
each arm used a fresh server, arm order rotated, and profiling ran separately.
Small-value and ordinary collection median throughput ratios were mostly
1.09–1.13, with lower p99 latency. P16/P64 ratios were 1.047/1.035; the 100k-key
ratio was 1.087. No client CPU warnings were reported. Million-key RSS was
215.57/215.64 MiB; smaller short-lived RSS samples varied with GC/runtime state.

Large-value throughput and single-client throughput remained near baseline.
The 1 MiB median p99 rose from 5.343 to 5.823 ms in this short run, while its
paired throughput ratio was 1.003 (range 0.972–1.016). Longer five-repetition,
15-second checks of small values, 1 MiB values, a single client and large-list
reads completed in [34103190849](https://github.com/brandopakel/keel/actions/runs/34103190849).
Small reads improved by a median 13.7% (all pairs 12.5–15.4%), with p99
0.367/0.319 ms. The 1 MiB p99 was 41.471 ms in both arms; its median throughput
ratio was 1.017, with a noisy 0.931–1.036 range. One-client throughput was 0.991
of baseline with identical 0.063 ms p99; large-list throughput was 0.996 with
6.015/6.143 ms p99. The short run's candidate-specific large-value tail increase
did not repeat, while both arms retained a substantial large-value tail.

Linux adoption is supported by the repeated small-workload improvement, preserved
readiness transitions, full correctness checks and no repeat material regression
in the other targeted workloads. Individual samples and unchanged/slower cases
remain in the evidence; they are not averaged into a universal gain.
The result is a measured gain in these hosted workloads, not a universal
speedup, append-overlap improvement or deployment capacity/SLO claim.

Raw reports and summaries are retained in
`bench/results/readiness-matched-2026-09-07.json.gz`. The separate candidate CPU
profile summary is in `bench/results/readiness-cpu-2026-09-07.txt`, and the longer
comparison is in `bench/results/readiness-targeted-2026-09-07.json.gz`.

The integrated 76ec59c run 34110455588 failed on Intel macOS: the slow-reader
check timed out while polling INFO to establish its retained-reply precondition
(the two-second PING check had not started). SIGQUIT showed the loop running on
another thread with its stack unavailable, so the cause is unresolved. A separate
no-TTL expiry test measured 742 ns against a 500 ns timing threshold. That test
now observes forbidden ActiveExpire calls directly, avoiding a hardware-dependent
correctness assertion. No runtime logic or slow-reader deadline changed. The
complete failed job log is retained in bench/results/readiness-intel-failure-2026-09-07.log.
A targeted diagnostic repeats the same slow-reader assertion on Linux and Intel
macOS; passing repetitions do not erase the original unexplained failure.

Diagnostic 34112834578 passed thirty isolated slow-reader repetitions on both
Linux and Intel macOS, but its separate full Intel job again timed out during
backpressure setup and in the existing unbuffered partial-reply test. The latter
captured the event loop in syscall.Write; the former again had no loop stack.
The full automatic PR job on the same c3e1049 revision passed. This discrepancy
remains unresolved and blocks adopting this branch on green status alone.
The second full failed job is retained in
`bench/results/readiness-intel-diagnostic-failure-2026-09-07.log`.

The final runtime limits registration caching to Linux. The controlled Intel
comparison in 34114096159 passed baseline at package parallelism 1 and 4 and
candidate at 1, but candidate at 4 failed the retained-reply setup again. Sampling
showed a 146 MiB footprint and the loop later returned to kqueue; these samples
do not prove the cause. Darwin now forwards registrations through the original
path, so the unproven Darwin optimization is deferred. Linux compile-time
behavior is unchanged from the measured runtime. Tests check both the cached
Linux sequence and the full Darwin registration sequence, including failed
registration retries and descriptor reuse. The complete failed diagnostic is
retained in `bench/results/readiness-contention-failure-2026-09-07.log`.

Final Linux-only automatic checks passed at b6b513bf, including native ARM64/Intel
and ext4/xfs recovery. The four-arm Intel diagnostic 34115455483 also passed
three complete suite repetitions in every arm after the platform restriction.
The optional ARM diagnostic now records a failed repetition and continues all
three fresh runs, then exits nonzero; its always-run artifact step retains every
completed result. This addresses the final outside-diff review finding without
changing server behavior or test deadlines.
