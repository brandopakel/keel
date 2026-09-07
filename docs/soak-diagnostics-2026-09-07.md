# Unresolved soak and Mac observations

The old `b9a97e0` continuous-primary run has stale progress at 10:29:35 UTC,
1,091,864 acknowledged writes and 739 checkpoints. Its two eight-hour recovery
siblings passed. The current `b14ffe0` runs are separate evidence and cannot
retroactively pass or explain the old stalled run.

Read-only inspection found established harness/primary/replica sockets with zero
receive/send queues, a regular-file harness console, and the primary/replica as
its only children. A sleeping native stack does not identify the Python statement
or prove that a server dropped readiness registration. The frozen RESP client
sets a three-second socket timeout; an eleven-hour blocked-read assertion needs
direct stack evidence rather than inference from established TCP connections.
A Python-level stack would have been needed for that hypothesis; attaching on
this Mac required root diagnostic access. After the user requested stopping the
48-hour soak before laptop sleep, the remaining old harness received SIGINT and
its owned processes exited. Its stale progress and terminal interruption report
are preserved. A live Python stack from that process is now unavailable. The
interruption does not resolve the original stall.

The newer `b14ffe0` continuous-primary run independently exited at 22:09:40 UTC
when its `ps` memory-monitoring subprocess exceeded the two-second deadline,
before this session sent any stop signal to that harness. It recorded 1,490,204
acknowledged writes and 96 replica crash recoveries. Both server INFO queries
still responded during failure capture; the follow-up `ps` calls also timed out.
The harness captured server stacks and stopped its children. This observation
must remain a failed incomplete run, rather than being relabeled as a successful
or user-interrupted run. The two completed eight-hour runs remain passed.
All continuous-run diagnostic records are archived in
`bench/results/frozen-continuous-soak-stop-2026-09-07.json.gz`.

PR 57 proposes a 20 ms multiplexer wait as defense in depth. Its description
currently overstates the evidence and recovery behavior. Both `b9a97e0` and the
current server already start a `CronIntervalMs` ticker that writes the wake pipe,
so periodic work does not inherently require external client traffic. A timeout
also does not itself restore an unregistered descriptor: empty event batches do
not re-register every client. Some queued/ordered-append paths may progress on
another loop turn, and the maintenance sweep may eventually close a stuck
client, but neither establishes a universal one-interval recovery guarantee.
A lost-registration root cause needs a captured registration/queue state or a
reproducer. The proposed timer needs that narrower contract and a measured
idle/ordinary workload cost. The original stalled run remains unexplained.

The historical Intel pending-reply timeout recurred in CI 34158319335. The later
matched diagnostic 34159834235 passed 400 focused subcases and six full suites;
those repeats do not explain the timeout. Read-progress counters and capture
before socket cleanup now preserve a more useful state if it recurs. The earlier
Apple Silicon fairness observation likewise remains unexplained despite
successful repeated diagnostics.

A new, separate Intel observation in CI 34162676036 missed the five-second startup
deadline replaying the roughly 195 MiB collection fixture. By the subsequent
stack capture the process had reached kqueue. The test now gives that large-file
recovery an explicit 30-second budget and logs readiness time; command deadlines
and ordinary startup are unchanged. Matched run 34163584482 passes 24 restarts,
266–332 ms baseline and 293–353 ms candidate. It did not reproduce the original
startup delay and does not establish its exact cause. Raw evidence remains in
the AOF-retention and command-allocation result archives.
