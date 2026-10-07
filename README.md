# Keel benchmark history

This branch holds data, not code. It keeps the results of Keel's benchmark
workflows after GitHub deletes their artifacts (30 days), so that a Grafana
dashboard can show them over months. `scripts/collect-bench-history.py` on
`develop` writes these files, and `.github/workflows/bench-history.yml` runs it
after each benchmark run and once a day. Don't edit the files by hand: a run is
recorded once, keyed by its artifact ID, and the collector rewrites each file
sorted by time.

Grafana reads the files through its Infinity data source, from
`https://raw.githubusercontent.com/brandopakel/keel/bench-history/<file>`.

Every file has `time` (the run's creation time, UTC), `branch`, `pr` (when the
commit heads a pull request) and `host`: the runner's CPU model, shortened
(`EPYC 7763`, `Xeon 8370C`), because results on hosted runners differ by host.
`artifact_id` and `run_id` link a line to its run.

## command-path/

The paired command-path benchmarks of `.github/workflows/command-path.yml`.
Ratios are candidate time over baseline time: below 1 is faster. The plan's
rule is a median of at least 0.98 across benchmarks; see the workflow's header
for the measured noise floor.

- `runs.csv`: one line per run. `median_ratio` is the median across
  benchmarks, with its 95% interval (`interval_low`, `interval_high`) and the
  range of the benchmarks' medians (`row_min`, `row_max`). `identical` is `yes`
  for an A/A run of byte-identical binaries. `failures` counts what the
  comparison failed on, such as more allocations per operation. `conclusion`
  is the job's result and `url` its page.
- `rows.csv`: one line per benchmark in each run: its median paired ratio, the
  95% interval, the range of its pairs, and ns/op, allocs/op and B/op on both
  sides.

Runs from before October 3, 2026 used an older comparison with no intervals,
pair ranges or B/op; those columns are empty for them, and their
`median_ratio` was taken from the benchmarks' medians in the same way.

## matched/

The matched keyspace adoption job of `.github/workflows/general-validation.yml`
(memtier against a baseline build and a candidate build on one runner).

- `runs.csv`: one line per run, failed ones included. `status` is `passed`,
  a suite's failing status, or `no summary` when the run produced none.
- `cases.csv`: one line per workload of each suite (`standard`, `memory`):
  the candidate/baseline throughput ratio's median and range (above 1 is
  faster), and the medians of ops/s, p99 and p999 latency and RSS on both
  sides. The memory suite measures RSS only.

The runs of September 7 and 8, 2026 were read from copies saved hours before
their artifacts expired.
