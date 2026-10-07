# Keel benchmark history

This branch holds data, not code. It keeps the results of Keel's benchmark
workflows after GitHub deletes their artifacts (30 days), so that a Grafana
dashboard can show them over months. `scripts/collect-bench-history.py` on
`develop` writes these files, and `.github/workflows/bench-history.yml` runs it
after each benchmark, census and telemetry run, and once a day. Don't edit the
files by hand: a run is recorded once, keyed by its artifact ID, and the
collector rewrites each file sorted by time. `census/latest.csv` is the one
file replaced rather than appended to.

Grafana reads the files through its Infinity data source, from
`https://raw.githubusercontent.com/brandopakel/keel/bench-history/<file>`.

Every file has `time` (the run's creation time, UTC) and `branch`, and
`run_id` links a line to its run. The benchmark and telemetry files also have
`pr` (when the commit heads a pull request) and `host`: the runner's CPU model,
shortened (`EPYC 7763`, `Xeon 8370C`), because results on hosted runners differ
by host.

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

## census/

The command census of `.github/workflows/command-census.yml`
(`scripts/command-census.py`): which of Redis's commands and subcommands Keel
has, as listed by Redis's `COMMAND DOCS`, with RedisBloom, RedisJSON and
RedisTimeSeries loaded. A command counts as present when Keel knows its name;
its options are not checked.

- `runs.csv`: one line per census, with `redis_version`, `commands` and
  `commands_present`, and `subcommands` and `subcommands_present`.
- `areas.csv`: the same counts per area of each census. An area is a command
  group (`string`, `list`, `pubsub` and so on) or a module (`module:bf`).
- `latest.csv`: every command and subcommand of the newest census of
  `develop`. `kind` is `command` or `subcommand` (written `container|sub`),
  `since` is the Redis version that added it, `deprecated` is `yes` or `no`,
  and `keel` is `present` or `missing`. It is replaced only by a newer census
  of `develop`.

## telemetry/

The Live telemetry workflow (`.github/workflows/telemetry.yml`), which runs
Keel and Redis under the same load with the standard `redis_exporter` in front
of each.

- `runs.csv`: one line per run, with how many metric names the exporter
  reported for Keel (`keel_metric_names`) and for Redis
  (`redis_metric_names`), how many both have (`shared_metric_names`), and the
  host. A failed run has empty counts.
- `phases.csv`: one line per load phase of each run (`steady`, `pipelined`,
  `eviction`, `expiry`), with each server's ops/s. Both servers share one
  unpinned runner, so these are not a comparison of speed.
