# Bounded client execution turns

A deep pipeline previously decoded and executed every complete command from a
socket read before another client could run. A large first response was also
copied into the shared reply arena even when the client could not drain it.

The parser now decodes at most 64 commands per turn. Execution yields after
64 commands, 64 KiB of replies, or a cooperative one-millisecond target checked
every eight commands. Unparsed bytes stay in the existing input buffer, while
unexecuted decoded commands retain their original order. A first reply of
at least 64 KiB owns its existing encoded buffer without an arena copy.

Each connection has at most one continuation queue entry. It is queued only
after prior replies drain, including their ordered persistence gate. Closed
connections and reused descriptors are checked by connection identity. Writable
events must still flush an earlier reply when decoded commands remain. This
last condition was caught by the existing partial-reply integration test during
implementation and fixed before publication. Queue references are cleared after
consumption; unusually large empty queue backing arrays are released.
The two flags fit existing padding: the client struct remains 200 bytes on
Darwin ARM64, measured against the integrated baseline.

The original implementation fails three new regressions: the 64-command execute
bound, the 64-command decode bound, and yielding before a large first reply's
arena copy. The candidate passes all three, plus connection-reuse and append-gate
continuation checks. A 1,025-INCR pipeline delivered in one write returns every
ordered reply without more writes, admits another client's PING, and preserves
the counter through two restarts. Tests cover one/four I/O threads and AOF-off,
worker barrier and ordered concurrent modes. Existing partial-reader cases pass
in buffered/unbuffered transports. Full local tests/vet and focused race pass.
Raw before/after checks are in `bench/results/client-fairness-2026-09-07.txt`.

The slow-reader test still establishes retained user-space replies before its
two-second PING watchdog. Its precondition is now 64 KiB instead of 16 MiB,
because yielding after one 1 MiB reply intentionally prevents construction of
the original 32 MiB batch. This is a changed retention assertion, not a relaxed
PING deadline. Hosted native/failure checks and matched workload adoption are
pending; local timings run alongside frozen soaks and are not capacity evidence.

Individual commands remain atomic and may take longer than the cooperative
target. A large command can produce up to the existing 64 MiB reply ceiling;
small preceding replies can coexist with it. This does not establish a fixed
latency SLA or a strict whole-process transient-allocation bound.
