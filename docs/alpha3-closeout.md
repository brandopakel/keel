# Alpha.3 release closeout and further validation

The owner authorized review closure, upgrade checks, merge/publication and further
benchmark/operational evidence on September 5, 2026. The earlier
[candidate record](pr15-validation-2026-09-05.md) identifies the original validation
and its limitations. This follow-up adds repeatable release gates.

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

Bencher/KVM, AWS and Grafana execution require the chosen existing account/host,
target, credentials through their normal secret store and a spending limit. The
portable adapters and provider packages are available; no account is provisioned
or spend incurred by these local/GitHub checks. Separate-host/deployment testing
will use the same evidence rules once those inputs are available.

Automatic failover, stronger acknowledgement protocols, concurrent execution during
appends, embedding and partitioning remain explicitly deferred engineering scope.
