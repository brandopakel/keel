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
