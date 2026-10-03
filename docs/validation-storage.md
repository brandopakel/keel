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
directories are redirected inside that directory. The compilation cache and
separate Go intermediate-build directory are removed after every run; other temporary files are removed on success and kept
on failure for diagnosis. Failed commands and resource stops never become passes.
All descendants in the wrapper's owned process group are stopped on exit.

The file ceiling is an inherited OS limit. Aggregate output and free-space checks
are sampled every quarter second, so they can overshoot between samples; this is
not a filesystem quota. The wrapper cannot contain a command that deliberately
creates a separate session or writes outside its designated directories. Use
owned validation scripts, never an existing application or production dataset.
No wrapper can guarantee a kernel-blocked process exits immediately.

### When a wrapper limit, not the code, stops a check

A limit hit is reported as the harness's, so it is not mistaken for a product
failure. The exit status is 0 for a pass, 1 for a failed command, 2 for
arguments refused before launch and 3 when one of the wrapper's limits stopped
or broke the command. The report's `limit_hit` and the last stderr line name the
limit, its value, the flag that changes it and the evidence:

| Limit | Flag | Evidence |
| --- | --- | --- |
| Time | `--seconds` (at most 120) | the wrapper stopped the command |
| Output budget | `--max-output-mib` (at most 1024, Go cache included) | the wrapper stopped the command |
| Free-space reserve | `--min-free-gib` (at least 2) | the wrapper stopped the command |
| Per-file size | `--max-file-mib` (at most the output budget) | below |

The kernel enforces the per-file ceiling, so the command sees it: a C program is
killed by `SIGXFSZ`, while Go and Python get `EFBIG` ("file too large") and
usually fail with that error in their output. A failed run counts as a per-file
hit when the command died of `SIGXFSZ`, when a file under the output directory
reached the ceiling, or when the command log reports a refused write while some
file was seen at half the ceiling or more. The last case covers a file that is
deleted after hitting the limit, as an abandoned AOF rewrite deletes its
`.rewrite` file. A shell's exit status 153 (128 + `SIGXFSZ`) counts as likely. A
refused write with no file near the ceiling is named as a `possible` hit and
keeps exit status 1. Some tests set a much smaller limit of their own
(`KEEL_TEST_FILE_LIMIT`). When another limit stopped the command, any
refused-write evidence is kept beside it, because a command can hit the file
limit first and then hang until the time limit. A run interrupted by its caller
(SIGINT or SIGTERM) is not diagnosed as a limit hit. If the calling shell's `ulimit -f` is below
`--max-file-mib`, the advice says to raise that limit instead. A passing run that
leaves a file exactly at the ceiling records a `file_limit_warning`. The kernel
shortens the write that crosses the ceiling without an error, so a command that
ignores a short count can still exit zero. Every report records
`peak_file_bytes` and `peak_file`, the largest file seen.

With the Go releases this repository supports (1.26 and later), `t.TempDir` is
created under `GOTMPDIR` when it is set. A test's temporary files therefore land
in the wrapper's `go-tmp` alongside the build work. They count toward its
budgets and are pruned with it, which `t.TempDir` cleanup does anyway. A fresh
Go cache plus `go test` build work for `./cmd/keel` measured about 290 MiB of the
512 MiB budget, which leaves about 220 MiB for a test's own files.

Keep tests within those budgets. `go test -v ./...` in the Go workflow's
ubuntu-latest/stable leg runs under `scripts/test-file-footprint.py`. That script
records every test's peak bytes on disk and its largest file in a job summary
table and a `test-file-footprint` artifact. It runs the command in its own process
group and stops what is left of it on exit or cancellation. A warning annotation is raised for a
test whose largest file reaches 128 MiB (half the per-file default) or that
holds 128 MiB on disk at once (a quarter of the output budget). The measurement
is sampled every 0.1 s. When a test genuinely needs more, skip it under
`testing.Short()` so brief local checks can pass `-short` while CI keeps running
it. Do not raise the defaults for it.

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

A directory removed concurrently during a sample is treated like a disappearing
file; permission and other traversal errors remain fatal. A deterministic scan
race test verifies that remaining files are still counted. Cleanup reaps an exited
parent before signaling its group, and a process test verifies surviving children
are still stopped. These checks follow an interrupted Go regression run whose
guard reported a vanished directory and a cleanup permission error. No owned
process remained. The latter error's exact cause is not established by a successful
repeat. The full Python test suite passed locally in 5.60 seconds; its cache and
temporary fixtures were pruned. Evidence is in
`bench/results/local-guard-temporary-churn-2026-09-07.json.gz`.

Go's `GOTMPDIR` is isolated from ordinary `TMPDIR`. Interrupted Go runs can leave
large intermediate archives even after `GOCACHE` is deleted; both Go directories
are now pruned on success or failure. Ordinary temporary failure evidence remains.
A regression verifies this distinction with a failing child command.

The September 7 draft evidence archive now holds 16 verified assets, including
the last 123 working report files. Their local copies and downloaded benchmark
ZIPs were removed after matching archive contents and GitHub digests. Thirty-nine
obsolete worktrees have been removed so far; active source workspaces remain.
The [archive index](../bench/results/local-evidence-archive-2026-09-07.json)
identifies the retained assets without adding bulky output to source clones.
