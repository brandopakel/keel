# Controlled append-cost comparison

`bench/run-append-diagnostics.py` uses the same exact runtime binary in both
arms. Baseline enables worker appends with the drained execution barrier;
candidate additionally enables ordered concurrent appends. Each policy (`no`,
`everysec`, `always`) uses identical fresh datasets, workloads, connection counts
and fsync settings. The paired runs rotate their order across repetitions.

The default cases cover balanced small commands, write-heavy commands,
pipelining and hashes, with three ten-second repetitions per arm. CPU/heap
profiles run separately on write-heavy traffic in each mode and each policy.
Profiled timings are diagnostic and excluded from the comparison summaries.
Reports include exact binaries, native-generator provenance, INFO persistence,
CPU/RSS samples and full raw memtier histograms. The workflow pins Go 1.27.1 and
memtier 2.5.1 and separates exposed server/generator core groups on one standard
Linux runner. Host tenancy and storage scheduling remain uncontrolled.

The local one-policy smoke completed its matched arms and both profile modes.
It ran alongside the frozen soaks, so its timing is not performance evidence.
The hosted comparison completed in
[34109092124](https://github.com/brandopakel/keel/actions/runs/34109092124), using
runtime 27cd4ad990994d0d4aeaa8d3f726ed88a8bbbf83 and SHA-256
7e6f9e50eaa912ded9ebf2daf4c44a84a26417398d66772641731c244898770f.
Server arguments were audited: both arms use the same binary, fsync policy and
worker; only the candidate enables concurrent append. No generator CPU warnings
occurred. Raw reports, histograms, build provenance and CPU-profile summaries
are retained in `bench/results/append-controlled-2026-09-07.json.gz`.

| Fsync | Workload | Median paired throughput ratio (range) | Median p99 barrier/concurrent, ms |
| --- | --- | ---: | ---: |
| no | balanced-64 | 1.262 (1.260–1.285) | 0.479 / 0.367 |
| no | write-64 | 1.248 (1.235–1.264) | 0.495 / 0.359 |
| no | pipeline-16 | 1.217 (1.199–1.259) | 0.767 / 0.679 |
| no | hash | 1.325 (1.312–1.374) | 0.471 / 0.327 |
| everysec | balanced-64 | 1.294 (1.292–1.317) | 0.487 / 0.351 |
| everysec | write-64 | 1.254 (1.199–1.273) | 0.495 / 0.367 |
| everysec | pipeline-16 | 1.223 (1.212–1.247) | 0.767 / 0.687 |
| everysec | hash | 1.364 (1.300–1.426) | 0.463 / 0.335 |
| always | balanced-64 | 0.860 (0.832–1.792) | 2.815 / 6.143 |
| always | write-64 | 0.841 (0.577–1.144) | 3.215 / 4.703 |
| always | pipeline-16 | 1.300 (0.932–1.370) | 10.559 / 3.903 |
| always | hash | 1.195 (1.182–1.469) | 1.599 / 1.247 |

These observations establish an overlap benefit for these `no`/`everysec`
workloads on this runner (22–36% at the paired median), with lower measured
p99. They do not establish a general `always` benefit: balanced/write medians
and tails regress, while pipeline/hash improve and paired ratios vary widely.
Host storage and tenancy were uncontrolled. Paired median ratios are not
ratios of independent medians; the complete pair distribution is retained.
This does not change which durability guarantee each policy provides.

Separate profiles put about 70–77% of CPU samples in syscalls. Under `no` and
`everysec`, barrier profiles spend about 22% cumulatively in readiness Monitor;
concurrent profiles still show substantial readiness registration and wakeup
costs. This supports investigating socket registration (PR #30). It does not
quantify per-operation savings: profiled arms complete different work counts.
CPU profiles also omit off-CPU fsync waiting, so they cannot explain the `always`
variance or justify a durability-policy change.
