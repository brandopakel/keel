# Persistence and replication implementation sequence

Status: proposed contracts and acceptance gates, September 6, 2026. The current
release and allocation candidate still use the one-batch append barrier and
bounded alpha replication. This document does not claim these stages shipped.

The measured command/allocation work comes first. Further persistence or
replication changes must preserve the same application workload and configured
acknowledgement policy when comparing performance.

## Ordered append execution

Keep command execution on its existing single owner. A single writer worker may
receive immutable byte batches; it must never read the maps, expiry metadata,
reply arena or mutable rewrite state. Track three monotonic logical positions:
encoded end, successfully appended end, and successfully synced end. A short
write advances only the appended prefix actually reported by the OS and makes
the writer fail closed. Positions are scoped to a log generation; rewrite file
offsets must not be mistaken for stable replication offsets.

Each reply batch records the position required by everything it observed.
For `always`, release at the synced position; for `everysec`/`no`, release at the
appended position. A read of an earlier unacknowledged mutation inherits that
mutation's barrier. Thus execution may proceed while append is running, but
success replies, read values, MONITOR output and replication publication cannot
expose a prefix earlier than its configured barrier. Closing a client does not
cancel an already executed mutation. A disconnected writer may have committed
without receiving its reply, as it can today.

Bound pending input, encoded AOF bytes and retained replies separately and in
aggregate. Queue entry count alone is insufficient for 1 MiB values. Stop
admitting commands before saturating the byte budget, continue servicing output,
completion and shutdown events, and resume fairly across clients when space is
available. Encoding and eviction effects need a preflight size bound: executing
a mutation first and then discovering an oversized canonical transcript is not
a complete admission design. Keep the current barrier until this is implemented.

On an append/sync error, latch the error, suppress all unreleased success batches
and stop serving the speculative state. There is no promise to roll back maps
in place. Recovery replays the valid persisted prefix. Shutdown drains or fails
the same ordered queue before closing descriptors. Rewrite finalization fences
all batches referring to the old generation, syncs the replacement and parent
directory, then switches the writer generation atomically.

Required fault tests use a controllable writer, not sleeps: pause an append,
execute dependent writes/reads, assert replies remain gated, saturate each byte
budget, disconnect clients, inject short writes and sync failures at every batch
boundary, request rewrite/shutdown, and replay the resulting file twice. Compare
acknowledged values and recorded prefix positions. Run race tests with workers
paused at each ownership transfer, plus matched latency tests under slow storage.

## Bounded traversal and serialization

Large-key work must be bounded by bytes and elapsed time as well as element
count. Lists already have an incremental rewrite path. Hashes, sets and sorted
sets need resumable traversal with mutation generations; a changed key restarts
with canonical deletion/replacement so historical fragments cannot win at
finalization. A single element larger than a chunk needs explicit size handling.

Whole-key name enumeration is another synchronous operation. Evaluate a stable
traversal/index abstraction shared by rewrite, full synchronization and SCAN.
Measure its per-key memory cost and mutation overhead at 100k and 1M keys before
adopting it. Copying every key for every SCAN request would hide an O(N) pause
behind a cursor-shaped API. An index or bounded cursor mechanism must specify
lifetime, abandoned-cursor cleanup, churn behavior and retained memory limits.

## Replication protocol beyond alpha

Version the new transport explicitly. Negotiate capabilities and retain clear
failure for older peers rather than interpreting a new frame as the alpha
protocol. Stream snapshots into bounded chunks with checksums, snapshot identity,
generation and a final commit marker. Publish the replacement dataset only after
all chunks validate and the catch-up prefix is complete. An interrupted snapshot
must never become the readable replica state.

Large-key changes need canonical operation records or chunked replacement with
generation fences. Operations with random results must carry the primary's
actual result. Expiry and eviction remain explicit primary decisions. Benchmarks
must record bytes transferred per logical mutation, snapshot peak RSS and pause
time for many-small-key and few-large-key datasets, including a dataset larger
than the current 8 MiB cap.

Resumption needs a durable tuple of primary identity, epoch, logical replication
position and locally committed state. Persist that tuple atomically with the
corresponding AOF prefix or snapshot manifest. Test crashes before and after each
write, sync, rename and directory sync. A checkpoint ahead of durable state can
silently skip mutations. If identity/history/checksum validation fails, perform
a fresh full synchronization. Local AOF byte offsets cannot serve as resumable
positions because rewrite changes them.

## Automatic failover

Manual promotion still requires external fencing. Automatic promotion is a
separate protocol: durable terms/votes, quorum membership and reconfiguration,
leader fencing, partition behavior and an explicit acknowledgement/loss policy.
No pair of independently writable nodes should infer authority from connection
loss alone. Acceptance includes old-primary reappearance, asymmetric partitions,
paused processes, concurrent elections, delayed messages and storage rollback.
Zero acknowledged-write loss would require a stronger replication acknowledgement
contract; asynchronous copies do not establish it.

## Evidence and deployment

Local APFS and public Linux CI provide useful correctness and diagnostic evidence.
They do not stand in for an isolated load/server host pair or deployment storage.
The $0 ceiling remains in force. Grafana and AWS login are already configured;
they do not establish an available free dedicated host. Run portable harnesses on
suitable existing hosts when available, retaining raw requests, errors, latency,
RSS/GC, transferred bytes, recovery times and acknowledged-loss checks.
