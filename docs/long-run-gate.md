# The long-run gate

The release checklist asks for 48 hours of continuous operation under crash
recovery. Three attempts on a laptop never completed, and the nightly soak that
PR #68 introduced in their place failed every night from September 9 to
September 17, 2026 - nine runs, 21 of 27 arm-jobs - on the growth bound it added.
This records what those failures were, what changed, and how the 48 hours are
now run without a machine that can be kept for two days.

## What the nightly failures were

Every failure was the same assertion, in two shapes. Neither was a leak.

**The replica's log under protocol 2** (18 of 18 arm-jobs, deterministic, five
minutes in). The bound was three times the largest value from cycles 1-3, which
put it at 18-45 MiB. The server does not compact a log below
`-auto-aof-rewrite-min-size`, then 64 MiB by default, and after a restart it
waits until the log has also doubled against a live-data estimate
(`internal/core/aof.go`). Under protocol 2 the replica keeps its log across
restarts, so it climbed through the bound while doing exactly what it was
configured to do. Because the run died first, the soak had never observed the
replica compact at all - the property the check was written to protect.

**The primary's log under protocol 1** (3 of 9 nights, at cycle 16, always
3.02-3.06x). The primary is rewritten at every 30-second checkpoint, so its log
swings between about 1.9 MiB and 5.7 MiB. The early cycles sampled the trough;
the checkpoint and recovery clocks then drift apart until a sample lands just
before a rewrite. The natural ratio between the two is the tolerance.

PR #68's validation, a 180-second run, could not have caught either: with the
default 60-second cycles that is at most three cycles, and the bound skips the
first three. The first nightly run was the first time it was ever evaluated.

## What changed in the harness

- Both servers run with `-auto-aof-rewrite-min-size 8mb`, so a multi-hour run
  compacts each log hundreds of times rather than never. The replica's
  compactions are counted in the report (`replica_compactions`), across its
  restarts, so "compaction happened" is a number rather than an inference.
- A log's baseline is floored at the server's compaction ceiling,
  `max(min size, 2 x 3 x used_memory)`, measured from the live data every
  cycle and only ever raised. Below that, growth is the contract; three times
  it is still crossed within a few cycles by a log that stops compacting. For
  this workload the ceiling is about 11.8 MiB, so the bound sits near 35 MiB
  against a healthy peak near 12 MiB.
- Temporary files have a floor of two. A quantity that settles at zero was
  never judged by the first version, so the descriptor half of the check had
  been a no-op. Torn-tail backups, which the server keeps on purpose after a
  crash tore the last record, are counted separately and not bounded.
- A run whose growth bound never judged a cycle fails rather than passing. The
  report records `growth.judged_cycles`.
- Every recovery cycle's measurements go to `recoveries.jsonl`, not only a
  breach, and each checkpoint carries the replica's persistence section.

Locally (M-series laptop, 2 MiB floor, replica restarted every cycle) the
replica did not compact until the log reached six times live memory - the
restart estimate, not the floor, decides - which is why the floor is the
ceiling and not the flag's value.

## The stall that motivated the gate

The one 48-hour attempt that ran long enough to stop was read at the time as a
server fault: a lost readiness registration leaving the event loop parked with
a reply owed. The preserved dump and the harness source that ran say otherwise
- the loop was being woken at 10 Hz, and the harness's three-second socket
timeout means it was never waiting on the server. It was asleep in a call of
its own with no deadline, most likely `ps`, which failed the same way on the
same laptop three days later. The [investigation](soak-progress-observability.md)
carries the evidence. The gate matters for the reason it always did - slow
growth and elapsed-time effects - not because that run found a defect.

## How 48 hours run on free runners

`long-soak.yml` runs each of the three soak shapes as a chain of nine segments
of 5h20m. A segment inherits both servers' logs and the replica's checkpoint
from the previous one, along with the harness's record of every value it was
ever acknowledged (`handoff.json`), verifies all of it on the new machine
before writing anything, runs the workload, and hands on. The final segment
promotes the replica, as the single-run soak does. Cumulative counters
(`cumulative` in each report) roll up across segments. The chain builds the
binary once and every segment checks its hash, and the harness refuses a
handoff from a different binary.

What this establishes: 48 hours of one dataset and one pair of logs under
continuous writes, crash recovery, compaction and eight planned restarts on
nine machines, with every acknowledged value verified at every boundary and at
every cycle. Growth is bounded by the check above at every cycle throughout.

What it does not establish: 48 hours of process uptime. A defect that appears
only after five hours of one process, and that neither the growth bounds nor
the crash cycles expose, is not covered. The nightly soak's "continuous
primary" arm covers 4.5 hours of it; the chain's boundaries restart both
servers, and the replica takes a full snapshot at each one because the
primary's replication history does not survive its restart. Say this when
citing the result.

The chain runs weekly (Saturday 04:00 UTC) and on dispatch. About 146 runner
hours per run, on standard runners in a public repository, which cost nothing.

## True uptime, when a machine exists

`scheduled-soak.yml` accepts a runner label and a timeout. On a self-hosted
runner the job limit is five days, so `runner=<label>`, `seconds=172800`,
`timeout_minutes=2940` is the uninterrupted 48-hour run the checklist
literally asks for. Candidate machines that cost nothing, in order of fit:

1. **Oracle Cloud Always Free** - an Ampere A1 shape up to 4 OCPU / 24 GB,
   permanently free, ARM. Needs an account with a card on file for identity
   only. The most capable option, and the one worth trying first.
2. **Google Cloud Always Free** - one `e2-micro` (shared vCPU, 1 GB) in a US
   region. Enough for this workload's ~2 MB dataset, marginal for the 1 MiB
   value's replication and the Go runtime under `gctrace=1`; try with the
   continuous-primary arm only.
3. **Any always-on machine you already own** - a NAS, a Raspberry Pi, a spare
   desktop. Register it as a runner with the label, and the same dispatch works.

Registering a self-hosted runner on a public repository exposes it to
workflows from forks unless "require approval for all outside collaborators"
stays on in the repository's Actions settings, which it should. Keep the
runner ephemeral or reset its work directory between runs. The zero-dollar
budget in `AGENTS.md` is why these are listed and nothing paid is.

## Reading the evidence

Each segment's artifact holds `report.json`, `checkpoints.jsonl`,
`recoveries.jsonl`, both `server.log`s, and - for a passing segment - the two
logs and checkpoint the next segment took over. `report.json` carries
`growth` (floors, settled values, judged cycles, breaches),
`replica_compactions`, `handoff_recovery` (how long the takeover verification
took and what the replica reported about it), and `cumulative`. A chain's
result is the final segment's report of each arm; anything else is a partial
run, and a partial run is not a pass.
