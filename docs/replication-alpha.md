# Experimental replication and asynchronous appends

Status: implemented in the working tree, not included in v0.1.0-alpha.2.
Both features are opt-in. This is a bounded first replication implementation for
small pilot datasets, not a capacity, high-availability or zero-loss guarantee.

## Asynchronous append mode

Add `-aof-async-append` to an AOF-enabled event-loop server. One immutable batch
is handed to an I/O worker. Command execution and expiry pause until that batch
has appended; `always` also syncs on the worker before releasing replies. Readiness
interests are temporarily removed to apply socket backpressure without spinning.
The loop continues accepting connections, checking timeouts and handling shutdown.
Replies are owned and accounted while waiting. Failures stop without releasing
staged successes. Shutdown joins the worker before closing the descriptor.

This first implementation deliberately has one in-flight batch, not concurrent
execution against unacknowledged state. A slow disk can still delay all commands.
Batches exceeding 64 MiB stop the server without acknowledgement; transient command
encoding can allocate before that limit. Rewrite serialization/writes/final sync
remain synchronous. A BGREWRITEAOF issued in the same batch as unflushed writes
returns a retry error; request it after receiving those writes' replies.

## Primary and read-only replica

Configure runtime secrets first, outside shell history/source control. Example
commands assume `KEEL_PASSWORD` and `PRIMARY_PASSWORD` are already set securely.
The addresses below belong on a private network; `-primary-tls` verifies TLS when
connecting through the primary's TLS proxy.

```sh
# Primary
./keel -host 10.0.1.10 -port 6379 -appendonly \
  -appendfilename ./primary.aof -requirepass-env KEEL_PASSWORD \
  -aof-async-append -replication-feed

# Replica (its own local client password can differ)
./keel -host 10.0.1.11 -port 6379 -appendonly \
  -appendfilename ./replica.aof -requirepass-env KEEL_PASSWORD \
  -aof-async-append -replicaof 10.0.1.10:6379 \
  -primary-password-env PRIMARY_PASSWORD
```

Replication requires AUTH, AOF and the normal event-loop mode on both nodes.
Chained replicas and replica memory eviction are not supported. Do not configure
`-maxmemory` on a replica; it must apply the primary's removals. Replica key-count
limits are not independently enforced. Primary admission/eviction remains the
source of truth. The replica follows primary expiry events rather than expiring
or evicting independently, so an expired value can remain visible during lag.

The primary exposes authenticated `KEEL.REPL.PULL epoch offset`. A frame contains
version, epoch, from/to offsets, full/delta designation, canonical RESP mutation
bytes and SHA-256 over payload plus metadata. Epochs change on process restart
or dirty-bookkeeping overflow; AOF rewriting does not change them. History retains
at most 16 MiB / 1024 batches. Falling behind retained history forces full sync.
Dirty bookkeeping is bounded to 100,000 key names / 8 MiB of name bytes; overflow
invalidates the epoch and forces a new snapshot instead of growing indefinitely.

The initial implementation admits up to 8 MiB of estimated keyspace, 100,000 keys
for a full snapshot, and 8 MiB encoded payload per frame. Larger datasets/frames
are refused by the feed; primary writes continue. These are explicit alpha limits.
Snapshot enumeration and canonical key serialization are synchronous. Incremental
updates replace affected keys rather than rerunning random commands: Morris,
Cuckoo and other probabilistic state therefore match the primary. This increases
bandwidth and serialization cost for large/hot keys; benchmark before adopting it.

The transport polls every 100 ms, receives at most one frame awaiting application,
and hands state changes to the replica's event loop. Commands in a frame are
structurally validated before application. An application/checksum/order failure
stops the replica, never continuing to serve a partial frame. A restarted replica
always obtains a fresh full sync before serving key reads, even if it has an AOF.
Client mutations receive READONLY. Before initial sync or after five seconds
without an applied primary update, key reads receive MASTERDOWN; PING/INFO remain
available. This freshness window is not a guaranteed RPO. INFO replication exposes
role, readiness, applied offset, update age and retained history bytes.

## Manual promotion and failure contract

1. Fence the old primary against client writes. For a planned promotion, keep its
   replica connection available while draining updates; then stop it. Stopping a
   local process in the integration test is not a distributed fencing system.
2. Before stopping a reachable primary, require zero `replication_pending_keys`,
   `replication_epoch_invalidated:false`, matching primary/replica epochs and
   matching primary/applied offsets in INFO replication. For an unplanned outage, accept that
   acknowledged primary writes absent from the replica can be lost.
3. Verify the replica completed full sync. Stop it cleanly and require exit status 0
   so its applied AOF is flushed. Do not promote a crashed or partially synchronized
   replica by merely removing a flag; recover/synchronize and validate it first.
4. Restart from that AOF without `-replicaof`/primary credentials, and optionally
   enable `-replication-feed` for new followers. Redirect clients only after checking
   the dataset and write behavior. Keep the old writer fenced.

There is no election, quorum, automated promotion, conflict resolution, WAIT command,
or guarantee against split brain after an operator removes fencing. Asynchronous
replication does not guarantee preservation of every acknowledged primary write.
AOF policies apply independently on both nodes. Stronger durability/failover
requirements require a different protocol and deployment commitment.

## Validation

Local tests cover canonical full/delta state across types, probabilistic dumps,
checksum rejection, offset gaps, history overflow, read-only and stale-read gates,
replica restart, and primary fencing followed by clean manual promotion. Async
append tests cover an injected blocking write/failure, real file-size-limit failure,
pipelined reply order, rewrite and restart under every fsync policy.
Native Linux/macOS CI, external latency/soak/fault runs and application pilots must
be recorded separately before promoting this experiment beyond alpha.
