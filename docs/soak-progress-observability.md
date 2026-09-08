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
`nanosleep` throughout. This does not identify the Python source line. Python
stack inspection required root access, unavailable through noninteractive sudo.
Stale workload time cannot count as a completed 48-hour continuous-write soak.
Evidence is retained in `bench/results/soak-progress-2026-09-07.json.gz`.

## The server side of that stall

Inspecting the preserved processes before they were released does identify a
Keel-side cause, which supersedes the statement above that none was established.

State at the time, eleven hours after the last checkpoint:

| | |
| --- | --- |
| harness, primary, replica | all alive, all at 0.0% CPU |
| all three TCP connections | `ESTABLISHED`, including the replication link |
| both append-only files | last written at the stall, not since |
| both server logs | nothing but `GC forced`, the two-minute idle collection |

A `SIGQUIT` to the primary wrote Go stacks to its existing log. The event loop
was in `io_multiplexing.(*KQueue).Check` at `server.go:568` - parked in `kevent`
- while a client held an established connection and an unanswered request.

That combination has one explanation. A descriptor was left unregistered, or
never re-registered, while a reply was still owed to it. The loop then has no
event to wait for and never turns again, so nothing retries, nothing times out,
and the harness waits on a reply that will not come. The harness sleeping is the
symptom; the server not waking is the cause.

`Check` waited with no timeout by design, which made this unrecoverable and also
meant the loop's own periodic work - the idle-client sweep and the
ordered-append maintenance tick - could not run on a server with nothing
arriving. PR #57 bounds that wait, so a lost registration now costs one interval
of latency instead of the process.

What is fixed and what is not, stated separately because they are different
claims:

- **Fixed:** the loop can no longer be permanently stuck. It turns on its own.
- **Not fixed:** whatever dropped the registration. The path was not identified,
  and the runtime has moved a long way since `b9a97e0`. A recurrence would now
  appear as a latency spike rather than a hang, which is harder to notice and
  worth watching for.

This is why a completed 48-hour run still matters as a release gate rather than
a formality: the one attempt that ran long enough to find this found a server
defect, not a harness one.

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

Both frozen eight-hour runs completed successfully at 12:17 UTC. Protocol 1
recorded 1,585,893 acknowledged cache writes; protocol 2 with concurrent appends
recorded 1,574,932. Each completed 951 checkpoints, 31 primary crashes and 64
replica crashes, plus final promotion/restarts and both RLIMIT_FSIZE write-failure
modes. Complete checkpoint/recovery series and terminal reports are retained in
`bench/results/recovery-eight-hours-2026-09-07.json.gz`. Their frozen source is
b9a97e002e5f6edb7fef54c7100ab9f7fd162e09, binary SHA-256
`aeed3178e90e12664f8f715a7adb2889c6bffcdf36fbba076cbe9d64862c8b31`.
These passes do not cover later changes or complete the stalled 48-hour test.
