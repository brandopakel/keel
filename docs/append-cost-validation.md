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
Hosted measurements must complete and be reviewed before claiming an overlap
gain. In particular, using different durability settings is not an append
optimization comparison. A result can show no gain or a regression and still
be a useful diagnosis.
