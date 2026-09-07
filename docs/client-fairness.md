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
PING deadline. Hosted native/failure checks passed; local timings run alongside frozen soaks and are not capacity evidence.

Individual commands remain atomic and may take longer than the cooperative
target. A large command can produce up to the existing 64 MiB reply ceiling;
small preceding replies can coexist with it. This does not establish a fixed
latency SLA or a strict whole-process transient-allocation bound.

The first hosted comparison in 34113254376 completed three ten-second
repetitions of six scenarios plus the nine-case memory matrix. Linux, Go 1.27.1,
AOF off and separate exposed server/generator core groups were matched. Small
reads and 1 MiB reads stayed near baseline (paired median ratios 1.009/1.015),
many-clients was 0.983, and large-list was 1.020. Pipeline-16 and pipeline-64
were both 0.968, with median p99 0.687/0.703 and 2.207/2.415 ms. No generator
CPU warnings occurred. The approximately 3% pipelined throughput cost remains
visible; the competing-client comparison below provides the corresponding benefit. Million-key
RSS was 217.73/219.93 MiB; the sample does not establish a memory reduction.
All raw measurements are in `bench/results/fairness-matched-2026-09-07.json.gz`.
The same commit's hosted Go/race, native/filesystem and broad checks passed.


The guarded competing-client repeat 34115851887 uses an independent generator
process and bounded queue per tenant, with both prepared at least 1,996 ms before
the shared start. Each offers 1,000 batches/s for 15 seconds, repeated three times
per runtime/case on the same Linux runner with disjoint exposed core groups.
The adversary sends 256 GETs of 64 KiB or 32 GETs of 1 MiB per batch; the observer
sends one GET of 64 bytes. AOF is off. All 24 tenant reports balance scheduled
arrivals against completed/failed/expired/dropped work, with zero protocol errors.
Generator CPU stays below 0.74 aggregate cores; latency includes queue delay.

| Adversary | Observer completed/s, before → after | Observer admission drops | Observer scheduled p99, ms | Observer service p99, ms |
| --- | --- | --- | --- | --- |
| 256 × 64 KiB | 344.8 → 1,000 | 65.52% → 0% | 69.21 → 1.36 | 28.57 → 0.41 |
| 32 × 1 MiB | 287.0 → 995.73 | 71.30% → 0.43% | 73.40 → 10.35 | 38.27 → 2.39 |

Values are medians across three repetitions, not a latency guarantee. Adversary
batch throughput also rises from 43.73 to 81.73 and 29.33 to 60.53/s, respectively;
it remains deliberately overloaded. Queue drops are generator admission drops,
not server rejections. An earlier run 34115181646 showed the same direction but
predated the explicit shared-start guard; its raw reports remain separate.
Both runs and exact runtime identities are retained in
`bench/results/fairness-competing-clients-2026-09-07.json.gz`.

Adoption accepts the approximately 3% ordinary pipelined throughput cost in
exchange for the measured competing-client progress and tail-latency benefit.
The fairness runtime was measured at 745ff7c4 against 2a34ce59. Later integration
adds independently validated maintenance fixes and Linux-only readiness caching;
final combined correctness checks remain required before release.
