# Alpha.3 release closeout and further validation

Release validation on September 5, 2026 covers upgrade/rollback, native installation,
matched benchmarks and bounded operational failures. The earlier
[candidate record](pr15-validation-2026-09-05.md) identifies the original validation
and its limitations. This follow-up adds repeatable release gates and their results.

The later [hosted validation record](post-alpha3-hosted-validation.md) adds actual
Bencher execution and further local recovery testing. The results and pending
inputs below describe the original release closeout.

## Review and source provenance

All 17 PR #15 review threads were assessed and resolved against fixes or the
documented alpha contracts. CodeRabbit's latest green status says **Review rate
limited**; it does not represent a fresh automated review of the closeout scripts.
Those scripts and workflow changes were manually inspected and exercised below.
The expanded validation ran at `764a43ff475b8b22b032c9d5c230cbff4f666230`;
subsequent closeout commits change documentation and evidence only. Tag publication
repeats tests, builds and native installation against the final tagged revision.

## Upgrade and rollback

`scripts/check-upgrade.py` seeds the published alpha.2 binary with strings, integers,
hashes, lists, sets, sorted sets, geo data and probabilistic structures. It preserves
an immutable AOF backup and compares values, TTL presence, counts and probabilistic
dumps through upgrade, candidate mutations, rewrite and restart. It then starts
alpha.2 from a separate copy of the backup and verifies the original state. It runs
all three fsync policies, each with candidate worker appends enabled and disabled.

```sh
python3 scripts/check-upgrade.py --baseline /path/to/alpha2/keel \
  --candidate /path/to/alpha3/keel --out /path/to/new/evidence-directory
```

The release workflow downloads the published alpha.2 archive for each target,
verifies both archives, executes their binaries natively, runs these six upgrade
cases and the documented Python client against the installed candidate. Targets:
Linux amd64/ARM64 and macOS Intel/ARM64. Runner labels follow the
[GitHub runner reference](https://docs.github.com/en/actions/reference/runners/github-hosted-runners).

The [release rehearsal](https://github.com/brandopakel/keel/actions/runs/33992676418)
passed all four native targets, **24 upgrade/restart/backup-rollback cases**, and
four documented client installations. Linux and macOS full tests, race tests and
vet also passed. [Machine-readable closeout evidence](alpha3-closeout-evidence.json)
retains native platforms and backup hashes. Publication was skipped for this run.

For an application rollout, stop/fence the previous writer before copying its AOF.
Retain the previous executable, configuration and a checksum-verified backup outside
the candidate's writable data directory. Validate the candidate against a working
copy before changing the application endpoint. If rolling back, stop/fence the new
writer and restore a copy of the preserved backup with the old executable.
This restores the backup's point in time: writes accepted after that backup require
application replay/reconstruction. In-place downgrade of an alpha.3-mutated log is
not the rollback contract tested here.

Tag publication creates a draft, uploads the four archives, downloads and verifies
their checksums, then publishes the prerelease. Native archive/upgrade/client checks
must pass first. Branch dispatch exercises the gates while skipping publication.

## Matched performance evidence

The Benchmark workflow's `suite=closeout` dispatch calls `closeout.yml`. Its benchmark
job builds alpha.2 and the candidate with the same Go toolchain on the same runner.
For each policy, five repetitions rotate/reverse baseline-sync, candidate-sync and
candidate-worker order. Each arm uses a fresh authenticated process, 15 seconds of
the same load and the same independently scheduled probe. `off` means AOF disabled.
All raw attempts, errors, dropped probes, binary/source metadata, host/filesystem
details, RSS/CPU samples and Go GC trace logs are retained.

```sh
python3 bench/run-paired-tail.py --baseline /path/to/baseline \
  --candidate /path/to/candidate --out /path/to/new/results
```

The load remains closed-loop and on the server host. Shared GitHub runner hardware,
Python scheduling and instrumentation affect results. `ps` CPU is a lifetime average;
RSS samples are in KiB. GC tracing and 0.5-second process sampling are enabled for
all arms. This controls toolchain, workload, placement and order without establishing
performance on dedicated separate-host hardware or an application deployment.

The [completed matched run](https://github.com/brandopakel/keel/actions/runs/33992676513)
contains **45 runs, 4,176,594 attempts, zero request errors and zero dropped probes**
on Linux amd64/ext4. Both source revisions used Go 1.27.1. The
[summary and raw-file hashes](../bench/results/alpha3-paired-summary.json) retain
all 90 load/probe measurements, RSS peaks and GC pause summaries. Median load
throughput across five repetitions is:

| AOF policy | Alpha.2 sync (ops/s) | Candidate sync (ops/s) | Candidate worker (ops/s) |
| --- | ---: | ---: | ---: |
| off (AOF disabled) | 6,901 | 6,853 | 6,882 |
| everysec | 6,867 | 6,741 | 6,334 |
| always | 4,821 | 4,785 | 4,607 |

The median of within-repetition worker/sync throughput ratios is 0.940 for
`everysec` and 0.964 for `always`. Probe tails vary, including between identical
AOF-disabled configurations. These results establish no worker-append speedup;
the option remains experimental and disabled by default.

The existing [Keel Bencher project](https://bencher.dev/perf/keel) now contains
separate alpha.2 and candidate reports, with their exact source hashes and 540
exported measurements. Report IDs and the shared GitHub runner testbed are retained
in the closeout evidence. The reports import the above GitHub measurements;
they do not represent execution on a Bencher/KVM runner. Publishing uses a local
credential store, with no project credential in the repository or GitHub secrets.

Large-list setup now batches RPUSH requests in groups of 256. Setup failures retain
an explicit failure row. The CSV `error` column remains error text, with only an empty
string meaning success; it is not a boolean flag.

## Operational evidence

`scripts/soak.py` performs authenticated writes and readback against a primary and
replica with worker appends and `always` fsync. The 15-minute Linux run checks
periodic rewrites, bounded list mutations, expiry, replica crashes/full resync,
primary crashes, stale-read rejection and canonical recovery. It ends with a clean
manual promotion after quiescing/fencing the old primary and checking matching epochs
and offsets. It retains checkpoints, RSS/CPU samples, GC traces and process logs.

File-size-limit failures exercise synchronous and worker append paths. The Linux job
also exhausts an isolated 16 MiB tmpfs to test real ENOSPC, refuses success replies,
then verifies previously acknowledged data through two restarts. Separate repeated
race tests cover injected sync errors and descriptor/reply barriers.

The [completed Linux job](https://github.com/brandopakel/keel/actions/runs/33992676513)
verified **315,676 acknowledged writes**, 29 rewrite checkpoints, four primary
crash recoveries, ten replica crash recoveries and a clean manual promotion over
901.6 seconds. Both append modes passed file-size-limit and real ENOSPC tests,
including recovery through two restarts. The separate race fault suite passed
20 repetitions. The normal persistence filesystem was ext4; ENOSPC used a private
16 MiB tmpfs. Local APFS upgrade and shorter operational checks also passed.

```sh
python3 scripts/soak.py --bin /path/to/keel --seconds 900 --out /path/to/new/evidence
```

These are bounded fault/soak tests on local APFS and GitHub Linux filesystems.
Hardware power-loss behavior, multi-day soak, network partitions across physical
hosts and storage faults on the eventual deployment filesystems need those targets.
Primary crash assertions use `always`; asynchronous replication still permits loss
of primary acknowledgements not yet present on a failed-over replica.

## External inputs still needed

Real pilots require an owner-selected application/workload and its latency, memory,
compatibility and loss/recovery acceptance criteria. No real application pilot or
outside outreach is implied by synthetic integration tests. Use [pilot-plan.md](pilot-plan.md)
for the intake and matched application comparison.

Bencher report ingestion is connected. Bencher/KVM execution, AWS and Grafana
execution still require chosen hosts/targets and a spending limit; AWS and Grafana
also need account access. The portable adapters and provider packages are available.
No paid benchmark host has been provisioned. Separate-host/deployment testing will
use the same evidence rules once those inputs are available.

Automatic failover, stronger acknowledgement protocols, concurrent execution during
appends, embedding and partitioning remain explicitly deferred engineering scope.
