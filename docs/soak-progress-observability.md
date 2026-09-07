# Soak progress and bounded harness failures

At 12:04 UTC on September 7, the frozen 48-hour run's process was alive, but
its checkpoint file had not advanced since 10:29:35 UTC: 739 checkpoints,
1,091,864 acknowledged cache writes and 74 replica recoveries. The two frozen
eight-hour runs continued advancing. None of these running processes, binaries
or harness files was restarted or modified during this inspection.

The old status helper checked process identity but not progress age. It therefore
reported the stalled run as running. `scripts/soak_status.py` now reports
`stalled_or_stale_progress` after 120 seconds without an updated report, includes
the report age, and refuses a pass without a terminal report and `passed: true`.
PID identity still prevents attributing a reused PID to a run. The installed
local status command uses this read-only observer.

A one-second native sample of the owned Python harness showed `time_sleep` /
`nanosleep` throughout. This does not identify the Python source line or establish
a Keel failure. The frozen primary and replica remained alive. Python stack
inspection required root access, unavailable through noninteractive sudo, so the
cause remains unresolved. Stale workload time cannot count as a completed
48-hour continuous-write soak. Evidence is retained in
`bench/results/soak-progress-2026-09-07.json.gz`.

New soak invocations use a main-thread signal watchdog, default 120 seconds,
refreshed after completed checkpoints and recoveries. A timeout dumps Python
stacks, raises through existing server diagnostics/cleanup and writes a failed
terminal report. Each final write-failure stage receives its own bounded window.
The telemetry `ps` command also has a two-second timeout. The watchdog refuses
to overwrite an existing real-time timer and restores the prior signal handler.
This is a Unix harness mechanism, not a Keel runtime change or hard kill-proof
guarantee for arbitrary uninterruptible native code.

Validation covers stale/live, missing/reused-process semantics, false pass
rejection, terminal failures, and an actual sleeping subprocess interrupted by
the watchdog with cleanup and stack evidence. A 20-second mixed replication
smoke passed, including recovery, promotion and write failures. A deliberately
short two-second watchdog failed a separate 30-second run before its first
checkpoint; it produced diagnostics and cleaned up both owned server processes.
The intentional failure is kept separately from successful evidence.
