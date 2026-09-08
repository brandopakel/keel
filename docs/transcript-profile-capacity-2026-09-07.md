# Transcript profiling and scheduled overload

These diagnostics explain where to investigate next. They do not establish a
general throughput improvement or dedicated-host capacity. GoGIF is unchanged.

## CPU and allocation profiles

[Run 34176103471](https://github.com/brandopakel/keel/actions/runs/34176103471)
passes 36 instrumented arms: three alternating pairs, two unchanged 64-byte
write workloads, and synchronous `no`, `everysec` and `always` persistence.
The candidate is `39043e3ed8bd8e54c147bb9a1fb6e966e11393f9`; the control is
`a1d550c1e3e73d2633e4097e45d1816cfe8e9e8c`. Both use Go 1.27.1,
memtier 2.5.1, fresh servers, 20-second measurements, 30-second shutdown grace
and disjoint exposed physical core groups on one public Linux VM per policy.
No generator CPU warning was emitted. CPU/heap capture and forced collections
perturb execution; these timings are excluded from the uninstrumented ratios
in [the transcript report](bounded-aof-transcripts.md).

For pipeline 64, median cumulative parser shares are:

| Policy | Control CPU | Candidate CPU | Control allocation bytes | Candidate allocation bytes |
| --- | ---: | ---: | ---: | ---: |
| no | 21.59% | 21.32% | 71.80% | 71.93% |
| everysec | 19.86% | 20.39% | 72.07% | 72.43% |
| always | 19.56% | 21.79% | 72.17% | 72.47% |

Parsing therefore remains a useful allocation target in both runtimes. The
candidate's AOF commit path accounts for 13.51%, 11.27% and 10.84% of CPU samples
in these three pipelined cells, respectively; its bounded command encoder
accounts for 7.13%, 5.63% and 4.55%, included within those commit totals. Key-map
lookups, syscall time and GC also appear in the profiles. The cumulative shares
overlap and cannot be added. They measure sampled CPU, not off-CPU filesystem
waits or a causal decomposition of the earlier tail-latency regression.

The new request-admission candidate validates a complete frame before copying
arguments and uses exact argument-slice capacity. Its own hosted diagnostics and
matched workload comparisons must establish the allocation gain and CPU cost.
It was not included in this profile run. Further encoder optimization needs
the same memory bounds, failed-write behavior and persistence order as control.

## Scheduled overload

[Run 34174358161](https://github.com/brandopakel/keel/actions/runs/34174358161)
completed 140 arms: seven cases, five scheduled rates, two repetitions and two
runtimes, ten seconds per arm, persistence disabled. Candidate `6966b55` predates
the failed-drain correction and the request-admission implementation. Control
is the same `a1d550c1e3e73d2633e4097e45d1816cfe8e9e8c` above.

All issued requests completed without recorded request failures. This is not a
zero-loss capacity result: bounded generator queues dropped scheduled work
under overload. Median observations at 150,000 scheduled requests/sec follow.
The p99 column is the median across repetitions of the greatest tenant p99;
it is not a merged request-level percentile for mixed tenants.

| Workload | Control completed/sec | Candidate completed/sec | Candidate queue drops | Candidate scheduled p99 ms |
| --- | ---: | ---: | ---: | ---: |
| read 64 bytes | 99,440 | 99,233 | 33.84% | 4.52 |
| balanced 1 KiB | 96,067 | 95,295 | 36.47% | 4.62 |
| expiry storm | 94,668 | 94,030 | 37.31% | 4.62 |
| 1 MiB values | 2,290 | 2,326 | 98.45% | 174.06 |
| large hash | 2,248 | 2,267 | 98.49% | 141.56 |
| large list | 4,409 | 4,211 | 97.19% | 80.74 |
| mixed tenants | 83,196 | 82,528 | 44.98% | 3.77 |

At 1,000/sec every case completed the offered rate without queue drops; all
small ordinary cases also did so at 10,000/sec. Large-value and collection
workloads saturate before 10,000/sec. Generator CPU use at the highest rate
ranged from roughly 0.9 core for 1 MiB values to 1.8 cores for mixed tenants,
on one exposed physical core with two SMT siblings. Generator scheduling,
response decoding, shared VM tenancy and server execution can all limit these
results. Queue drops belong to generator admission and are not Keel rejections.
Two repetitions do not establish the cause of the large-list variation.

Source/build provenance, per-tenant service/scheduled/queue latency, generator
CPU, profiles and raw reports are retained in
`bench/results/transcript-profiles-2026-09-07.tar.gz` and
`bench/results/transcript-capacity-2026-09-07.tar.gz`. Their checksums and parsed
rows are in `bench/results/transcript-profile-capacity-summary-2026-09-07.json.gz`.
Successful disposable AOFs were hashed and pruned on GitHub runners.
