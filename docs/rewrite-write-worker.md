# Bulk snapshot writes on an owned worker

Status: candidate undergoing validation, September 7, 2026.

Bulk rewrite slices previously called File.Write on the command owner. A bounded
slice limits bytes copied, but a slow filesystem can block that call indefinitely.
The candidate transfers one immutable encoded slice to a replacement-file worker.
The owner continues commands and records dirty keys in the original AOF while
the worker is blocked. It cannot enqueue a second slice, reuse the descriptor,
start a sync or reuse the temporary path until the first job completes.

The worker never visits a store, cursor, borrowed sketch table or live digest.
On completion the owner updates the digest and written-byte count. Completion
wakes the event loop; an incomplete job does not cause a self-wakeup loop.
INFO persistence exposes `aof_rewrite_pending_write_bytes`, charging the retained
slice capacity rather than only its length. Other retained rewrite state remains
separate; this metric is not a process-wide allocation reservation.

Four timing groups distinguish `aof_write`, `aof_sync`, `aof_rewrite_write` and
`aof_rewrite_sync`. Each exposes `_inflight`, `_calls`, `_errors`, `_total_usec`,
`_max_usec` and `_last_usec`. Fixed atomic counters span the process lifetime;
individual fields are approximate concurrent snapshots. Durations measure elapsed
wall time inside the filesystem call, not CPU time or reply acknowledgment latency.
The later finalization group measures its complete handoff phase.
Short writes count as failures. These observations can distinguish write stalls
from sync stalls in future overload reports; existing reports cannot be assigned
a cause retroactively. A local empty-call diagnostic measures roughly 53–73 ns per
timed call and zero allocations, not whole-server throughput overhead.

Cancellation transfers close/remove responsibility to a blocked worker. New
rewrites refuse the path until cleanup finishes, and shutdown joins outstanding
work. A short or failed replacement write abandons the rewrite and leaves the
original AOF available for continued writes and restart recovery.

Only the bulk snapshot write phase moves. Once the bulk walk finishes, its last
write completes and the existing asynchronous preflush runs. Dirty reconciliation
then uses the existing synchronous writes and final sync/close/rename/directory
sync handoff. An initial prototype also queued every dirty-tail write; sustained
updates could then chase the same key indefinitely. The retained failing growth
test and corrected phase transition explain why that broader change is withheld.
Moving the final phase needs a separate ordered dual-write and commit-cut design.

Controlled tests hold a replacement write while 100 SET/GET pairs and original
AOF flushes complete, assert one retained job and no polling wakeups, then verify
the final value through two replays. Additional tests inject failed and short
writes, cancel an in-flight write, refuse premature path reuse and verify cleanup
and acknowledged values. Existing sync, mutation, automatic-growth and oversized
record tests run against the new worker path.

Worker scheduling can increase whole-rewrite duration; a byte limit is not a
latency guarantee. Matched hosted interference results and integrated native
validation are required before adoption. No frozen-soak or release success is
attributed to this candidate.

Hosted string interference run 34158319501 compares `ec8aa5a` (worker, before the
new timing counters and sketch integration) with corrected-term baseline
`ce12e79`. All 36 arms complete 108 rewrites and 1,080,000 scheduled requests with
zero failed, dropped or expired requests. Across six durability/write cells,
median scheduled p99.9 is 3.93–4.72 ms in the baseline and 1.59–3.67 ms with the
worker. Median rewrite duration rises from 46–50 ms to 52–53 ms. This trades a
slightly longer bulk rewrite for lower measured interference on that public VM;
it does not bound slow-filesystem latency or remove final handoff stalls.
Raw records are in `bench/results/rewrite-write-worker-hosted-2026-09-07.json.gz`.

The combined worker, timing counters, borrowed sketch streams and bounded hash
storage pass the full local suite and focused rewrite/sketch/persistence race
checks at `6699a8d`. These integrated local checks do not establish hosted
performance or qualify the frozen soak binaries for the new runtime.

The integrated runtime `f47348e` also includes sorted-set compaction and the final
sketch documentation. Full local tests, vet and focused rewrite/I/O race tests
pass. Run 34160469172 measures all three rewrite datasets at 500 requests/s
against merged baseline `135ccbe87d169b75314223f1c6d2b684e5ab5272`, retaining the
new filesystem timing counters even when traffic fails its gate. Its separate
ordinary matrix failed setup because the dispatch supplied three nonexistent
scenario names; that job did not establish performance. A follow-up dispatch
34160718591 supplied an incorrect baseline SHA and was canceled. Corrected
ordinary run 34160763394 uses the verified baseline and existing scenario names.
These dispatch errors are separate from measured service/traffic failures.

Corrected ordinary run 34160763394 completes 50 arms (five pairs, 15 seconds,
five workloads), with no generator CPU warnings. Paired throughput ratios are
1.006 for small reads, 1.000 for many clients and hashes, 0.993 for pipeline-64,
and 0.996 for sorted sets. Median p99 is unchanged except the many-client case
(4.255 to 4.063 ms). These are near-neutral ordinary results, including a small
pipeline cost, not a general throughput gain.

The integrated 500-request/s CMS and Morris matrices each complete 36 arms,
108 rewrites and 270,000 requests with no errors or drops. Adding the worker to
the already-streaming sketch baseline has mixed tail effects; no additional
sketch improvement is established. Strings drop 21 baseline and 15 candidate
requests in the first always-sync/20%-write pair. Other string policy cells have
median p99.9 4.19–5.05 ms baseline versus 1.62–2.72 ms candidate. The failed cell
is not adopted on its successful repeats. All raw traffic and timing records
are retained in `rewrite-worker-integrated-interference-2026-09-07.json.gz`.

In the failed candidate arm, rewrite sync's maximum grows from zero to 148,966
microseconds, establishing a long rewrite sync during measurement. AOF sync's
173,890-microsecond maximum is already present before measurement and cannot be
attributed to that traffic interval. The aggregate counters alone cannot tell
whether the long rewrite sync was bulk preflush or final handoff. Additional
counters now distinguish `aof_rewrite_final_sync` (a subset of rewrite sync) and
`aof_rewrite_finalize` (the complete final synchronous handoff). Do not sum these
nested durations. Each group also records calls lasting at least 10 ms via
`_slow_calls`, `_slow_last_usec` and completion time `_slow_last_unix_usec`, so an
older maximum cannot hide a subsequent slow interval. Fields remain approximate
independent atomic observations. These new fields pass local full/vet and three
rewrite/I/O race repetitions; they do not retroactively assign a cause to older
observations or remove the handoff stall.
