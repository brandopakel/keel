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
reads are pending in [34103190849](https://github.com/brandopakel/keel/actions/runs/34103190849).
The result is a measured gain in these hosted workloads, not a universal
speedup, append-overlap improvement or deployment capacity/SLO claim.

Raw reports and summaries are retained in
`bench/results/readiness-matched-2026-09-07.json.gz`. The separate candidate CPU
profile summary is in `bench/results/readiness-cpu-2026-09-07.txt`.
