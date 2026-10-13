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

The same workflow runs the argument census (`scripts/argument-census.py`).
It sends every argument form Redis's own command specifications document
(`src/commands/*.json` in the Redis source) to Keel and to Redis, for every
command Keel has, and compares the replies. A form is the base form, or the
base form with one documented option, alternative or repetition added.

- `argument-runs.csv`: one line per census:
  - `commands` and `forms`: how many commands and subcommands Keel has, and
    how many forms they document.
  - `accepted`: forms Keel answers as Redis does.
  - `refused`: forms Keel answers with an error where Redis answers a value.
  - `wrong_reply`: forms both answer, differently.
  - `redis8_forms` and `redis8_accepted`: the forms Redis 8 added, and how
    many of them Keel accepts.
  - `no_reply` and `control`: failures of the method itself, which fail the
    run. A control failure is a form Redis itself refuses.
- `argument-areas.csv`: the same counts per command group of each census.
- `arguments.csv`: every form of the newest census of `develop`:
  - `command`, and `form`: the option exercised, or `base`.
  - `since`: the Redis version that added the option.
  - `sent`: the command as sent, with the password shown as `<password>`.
  - `class` and `detail`.
  - `keel` and `redis`: both replies, in a short protocol notation (`+OK`,
    `:1`, `$abc`, `*[...]`, `-ERR ...`).

  It is replaced only by a newer census of `develop`. A census from before
  the argument census (October 12, 2026) has no lines in these files.

The reply census (`scripts/reply-census.py`) checks the shape of what Keel
answers. A case is one documented argument form, or the base form with
missing keys, with a key of another type, or with one argument too many. Each
case is sent in RESP2 and again in RESP3. A reply's shape is its protocol
types with the values removed (`*[$ :]`, `%{$::}`, `_`). Each RESP3 reply is
also checked against the `reply_schema` Redis keeps for the command.

- `reply-runs.csv`: one line per census:
  - `commands` and `cases`: commands and subcommands Keel has, and the cases
    sent, each protocol counted once.
  - `same`: cases where Keel's reply has Redis's shape.
  - `differs`: cases where it does not.
  - `refused`: forms Keel refuses. The argument census counts these, so they
    are not shape failures.
  - `no_reply`: a failure of the method itself.
  - `resp3_cases` and `resp3_same`: the RESP3 half of the cases.
  - `keel_schema_violations` and `redis_schema_violations`: RESP3 replies
    that break Redis's own reply schema. Redis's count is the control: a
    violation there is a flaw in Redis's schema.
- `reply-areas.csv`: the same counts per command group of each census.
- `replies.csv`: every case of the newest census of `develop`:
  - `case` (`form`, `missing key`, `wrong type` or `extra argument`),
    `form` and `protocol` (`2` or `3`).
  - `sent` and `result`.
  - `keel_shape` and `redis_shape`.
  - `keel` and `redis`: both replies.
  - `keel_schema` and `redis_schema`: `ok`, `error`, `no schema`, or the
    violation found. They are empty on RESP2 lines, which are not checked.
  - `redis_function` and `redis_at`: the Redis function that answers the
    command, and its `file:line` in the 8.10.1 source.

  It is replaced only by a newer census of `develop`.

The configuration census (`scripts/config-census.py`) compares Redis's
configuration table (`src/config.c`) with Keel. For each directive it checks
`redis.conf`, and asks both servers `CONFIG GET` for it.

- `config-runs.csv`: one line per census:
  - `configs`: Redis's directives in this build.
  - `has`: directives Keel reports through `CONFIG GET` under Redis's name.
  - `mapped`: directives a Keel flag covers under another meaning.
  - `lacks`: directives Keel lacks. Of those, `lacks_used` are read or set
    by a client, exporter or operator the census checks, and `lacks_tested`
    are set by Redis's own test suite.
  - `documented`: directives `redis.conf` shows.
  - `same_value`: directives Keel has, with Redis's value.
- `config.csv`: every directive of the newest census of `develop`:
  - `name`, `alias`, `family`, `kind` and `flags`, the last two from
    `src/config.c`.
  - `documented`: `active`, `example` or `no` in `redis.conf`.
  - `class`: `has`, `mapped` or `lacks`.
  - `keel_value` and `redis_value`: each server's `CONFIG GET` answer,
    empty if it has none.
  - `note`: for a mapped directive, the Keel flag and how it differs.
  - `used_by`: who uses the directive, with the evidence.
  - `test_files`: how many files of Redis's test suite set it.

  It is replaced only by a newer census of `develop`.

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
