# Validation with a small local footprint

Use GitHub Actions for full suites, race/platform coverage, workload matrices,
capacity sweeps, extended fuzzing and operational recovery runs. Local work is
limited to short, targeted checks; no detached or overnight local test is needed.
Keep reports and reproducible source on GitHub, then prune generated local files.

## Short local checks

```sh
python3 scripts/run-local-validation.py --out dist/local-check --seconds 120 -- \
  go test ./internal/core -run '^TestSpecificRegression$' -count=1

python3 scripts/run-local-validation.py --out dist/local-smoke --seconds 60 -- \
  python3 bench/run-general.py --candidate /path/to/keel --out '{out}/workload' \
  --cases cache-read-64 --reps 1 --seconds 1
```

The wrapper creates a fresh directory, captures stdout/stderr and writes a
`local-resource-report.json`. Its default command deadline is 120 seconds, output
budget 512 MiB, per-file limit 256 MiB and free-space reserve 10 GiB. It refuses
unreadable output directories and reserves 64 KiB inside the output budget for
its final status report. Inherited soft and hard file limits are never raised.
After command and descendant exit, it rechecks all limits before recording a pass.
It refuses
deadlines above two minutes or output budgets above 1 GiB. Go caches and temporary
directories are redirected inside that directory. The compilation cache is
removed after every run; other temporary files are removed on success and kept
on failure for diagnosis. Failed commands and resource stops never become passes.
All descendants in the wrapper's owned process group are stopped on exit.

The file ceiling is an inherited OS limit. Aggregate output and free-space checks
are sampled every quarter second, so they can overshoot between samples; this is
not a filesystem quota. The wrapper cannot contain a command that deliberately
creates a separate session or writes outside its designated directories. Use
owned validation scripts, never an existing application or production dataset.
No wrapper can guarantee a kernel-blocked process exits immediately.

The general benchmark runner now hashes and removes successfully stopped Keel
AOFs by default. `--retain-passed-aof` is an explicit diagnostic opt-in. Failed
AOFs remain intact. A checksum and pending-removal record are saved before unlink;
the final report records whether removal completed. Removing the file changes
neither the measured workload nor its durability policy.

## Evidence and retention

1. Keep exact source/binary/harness identities, workload parameters, reports,
   latency samples, checksums, warnings and failure diagnostics. Preserve failed
   or partial results alongside successful repetitions.
2. Hosted jobs upload bounded evidence artifacts even after failure. Keep durable
   compact result archives and conclusions under `bench/results/` and `docs/`;
   Actions artifacts expire according to their retention policy.
3. Verify the result commit exists on GitHub before removing unique local
   evidence. Large successful generated AOFs and reproducible build caches are
   disposable once their reports and checksums exist. Preserve the diagnostic
   contents of failed data before deleting it.
4. Remove obsolete clean worktrees only after checking remote commit reachability
   and preserving unique uncommitted probes. Keep the active source checkout and
   current review worktrees. Do not clean another active session's workspace.

Do not publish credentials, environment dumps, personal disk inventories, unrelated
application data, build caches or raw multi-gigabyte logs. Keep spending at $0;
existing free public GitHub runners provide validation, not dedicated-host
capacity guarantees. A long local soak is not part of the remaining plan.

The free-space preflight and final checks include 64 KiB for the final report. If
a command makes its output root unreadable, the wrapper restores access to the
original directory before cleanup. It never overwrites a command-created report
path; a collision is a failed run with a sibling fallback report and JSON on
stdout. Filesystem errors can prevent any on-disk report, so stdout remains useful.
This cooperative guard cannot prevent unrelated processes consuming disk or a
command intentionally escaping its process group/output directory.
