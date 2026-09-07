# Streaming replication experiment (merged, unreleased)

Protocol 2 is merged development, absent from alpha.3 archives, selected with `-replication-protocol 2` on
both the authenticated AOF primary and its read-only replica. Protocol 1 remains
the default. Use the same connection and secret configuration shown in
[the alpha replication guide](replication-alpha.md), adding this flag to both
commands. An explicit `KEEL.REPL.PULL2` command rejects mismatched peers.
There is no automatic negotiation or failover.

## Snapshot and update contract

The primary creates an incremental AOF rewrite, then retains a read-only file
descriptor and the completed file's prefix length. That prefix is immutable
even as later appends or rewrites proceed. A snapshot identity, primary epoch,
logical stream position, byte position, total length and completion marker
accompany each chunk. Each frame checksums its metadata and payload together.
The replica accepts only contiguous chunks from the same snapshot. It gates key
reads before replacing the old dataset, and keeps them gated through snapshot
completion until the primary confirms catch-up.

After the snapshot, ordinary strings, hashes, lists, sets, sorted sets and geo
updates carry canonical AOF operations. Random pops and sorted-set increments
use their recorded results. Probabilistic structures carry exact replacement
images to preserve random seeds and internal state. Expiry and eviction remain
primary decisions; a delayed expiry record must not locally delete a counter
before a following increment.

The stream keeps at most 16 MiB of payload history, packed into pieces of up to
256 KiB. It does not allocate one history entry per small command. Snapshot
chunks and delta frames carry at most 256 KiB of payload, excluding JSON/base64
and RESP framing. Positions count canonical stream bytes and survive rewrites.
History overrun, a new primary epoch or an invalid local recovery checkpoint
requires a new full snapshot. A continuously changing dataset can outpace this
history window and require repeated snapshots; there is no guaranteed catch-up
rate under arbitrary load.

Snapshots are limited to 1 GiB of encoded AOF and the existing rewrite limit of
one million keys. The receiver buffers at most 64 MiB for incomplete canonical
operations. These bounds are experimental limits, not measured capacity claims.
A single value or opaque image still requires synchronous serialization and
may exceed the command limit. Such a transfer cannot establish a readable
replica. Hash/list/set/sorted-set rewrites yield after at most 256 entries, a
roughly 64 KiB encoding target or a one-millisecond work slice. One oversized
entry streams in 64 KiB fragments. Hash traversal keeps one map cursor,
discarded on mutation, without adding a per-field index. Initial key names use
bounded stable-slot batches. Opaque image construction, file writes and final
file/directory syncs can still stall command execution.

## Restart checkpoints and rollback

A caught-up replica forces its AOF to disk before atomically publishing
`<aof-path>.replica-checkpoint`. The private sidecar names the configured primary,
epoch, logical replication offset, exact AOF length and SHA-256. Startup hashes
the local AOF and accepts the checkpoint only if all fields match. It still
withholds reads until the primary confirms catch-up. New suffixes, repaired or
truncated logs, changed contents, a different primary, or damaged/missing
metadata cause full synchronization. A rewrite maintains a fresh incremental
digest; the next catch-up checkpoint names that new file generation.

This adds local sync work even with `appendfsync everysec` or `no`, and hashing
adds one sequential AOF read at restart. These costs must be included in any
matched durability/performance comparison. A checkpoint failure stops replica
service; it does not authorize reads of partially persisted state.

A dropped network connection resumes an in-memory snapshot cursor. A process
crash during an unfinished snapshot starts a new snapshot. Restart resumption
works only from a complete validated checkpoint while the primary's epoch and
history survive. The history itself is not persisted across primary restarts.

The AOF continues to use existing canonical records. To roll back, fence the
primary, quiesce and verify the replica, stop both cleanly, and retain copies of
the AOFs and checkpoint sidecars. Restart the older binaries with their supported
flags and protocol 1; they ignore the sidecar and obtain a fresh replication
snapshot. An incomplete replica must be synchronized and validated before manual
promotion. No checkpoint changes asynchronous replication's potential data loss
after permanent primary failure.

## Reproducible validation

```sh
go test ./...
go vet ./...
go test -race ./...
go build -trimpath -o dist/keel-candidate ./cmd/keel
python3 scripts/check-replication-v2.py --bin dist/keel-candidate \
  --out dist/replication-v2 --concurrent
python3 scripts/soak.py --bin dist/keel-candidate --out dist/soak-v2 \
  --seconds 900 --cycle-seconds 60 --concurrent --replication-protocol 2
```

The unit tests cover snapshots above 8 MiB, exact probabilistic state, operation
ordering, chunk gaps/corruption, incomplete catch-up, old-protocol rejection,
history overflow, expiry across restart, changed/truncated AOFs, bounded metadata
reads, and AOF/metadata sync, rename and directory-sync failures. Collection
rewrite tests mutate, delete and replace keys mid-traversal and replay twice.

The separate-process harness uses 32 MiB of string values and three 2,000-member
collections. It interrupts a real TCP snapshot response, compares full state,
checks small collection deltas and restart resumption, then forces history
overrun, local AOF alteration and a primary restart. Reports retain binary and
harness SHA-256 values, transfer bytes and recovery durations. These are local
functional measurements, not dedicated-host throughput results.

On the local M4 Pro, a 100,000-field traversal microbenchmark allocated
3,211,264 bytes for materialized field/value slices versus 208 bytes for the
cursor. Median full-traversal time increased from 0.982 ms to 1.608 ms across
three repetitions. The cursor trades additional traversal CPU for bounded
temporary allocation and the ability to yield; this is not a throughput speedup
or a measured network latency improvement. A recovery soak shared the machine
during this diagnostic. Raw results are in
[the traversal benchmark](../bench/results/hash-traversal-2026-09-06.txt).

The new GitHub workflow runs this harness, crash recovery and release archive
upgrade/rollback on native Linux ARM64 and Intel Mac runners. It also runs short
recovery workloads on owned ext4 and XFS loop filesystems. Standard runners are
free for public repositories under [GitHub's runner policy](https://docs.github.com/en/actions/reference/runners/github-hosted-runners).
CI results, long-soak results and dedicated deployment performance must each be
reported separately; preparing a workflow or starting a soak is not a pass.
