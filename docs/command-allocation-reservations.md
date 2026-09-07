# Command reply and workspace reservations

Status: candidate, September 7, 2026. Depends on GEO admission in PR 53.

The event loop now reserves covered command allocations against its 256 MiB
buffer budget and separate 192 MiB reply-class budget before constructing them. The starting charge includes retained
client input/replies, AOF buffers and arena capacity. Each command in the same
execution run also sees AOF/arena growth from earlier commands. Reservations
accumulate for the run and release when it returns; the transport then accounts
its retained output before another run executes. Large output reserves three
page-rounded payloads for construction, arena copying and detaching a partial
reply. This is deliberately conservative; arena windows can be charged twice.

Covered allocations are GET, SET GET, PING echoes, HGET, LINDEX, MGET/HMGET,
collection traversal replies, destructive pop output/member arrays and canonical
SREM/ZREM records, repeated random-member indexes, DUMP replies, GEOSEARCH point
selection/output, LCS computation/output, and KEYS/SCAN name batches/output.
The per-command 64 MiB encoded reply limit still applies. SET GET reserves and
encodes the previous value before replacement. Pops reserve before removing
members. A refusal returns an explicit error and leaves logical membership and
values unchanged; normal lazy expiry and random-set position shuffling retain
their existing behavior.

KEYS now traverses bounded name batches twice instead of first materializing all
key names and matches. It still scans the full dataset and can stall execution.
SCAN keeps its existing cursor/work semantics but sizes its nested reply before
encoding directly into one buffer. Both refuse oversized key names before
constructing a large encoded reply. LCS retains its CPU guard and conservatively
reserves reversed inputs, rows, pair/run growth and optional nested index output.

INFO clients reports `command_allocation_limit_bytes`,
`command_allocation_reserved_bytes`, `command_allocation_peak_bytes` and
`command_allocation_refusals`. Peak is the maximum observed retained-plus-reserved charge,
including retained buffers, rather than measured heap or RSS. Small fixed error
and control replies remain available on refusal. Disconnecting/draining slow
readers restores space for a retry.

This is **not a complete process-wide temporary allocation pool**. Parsing,
unmodeled write/canonical/eviction growth, replication history and transport
buffers, rewrite images/retired borrowed values, compaction overlap, alternate
transports, allocator metadata and kernel memory remain outside this reservation
contract. Existing append ordering/barrier and class limits still apply. Extending
coverage requires reserving before mutation or a separate bounded persistence
stream contract; returning an allocation error after a write is not acceptable.

Validation passes overflow/aggregate checks, before-allocation read refusals,
joint GEO workspace/output admission, LCS workspace refusal, and two exact AOF
replays after refused SET GET and list/set/sorted-set pops. A process test leaves
24 slow readers holding at least 160 MiB of replies, observes refusal of a 32 MiB
LRANGE, continues small commands, disconnects the readers and receives every
original list member on retry. Three process repetitions pass. The full local
suite, vet and three focused race repetitions pass; an initial test constructor
typo is retained in the evidence archive. Hosted correctness, review and matched
ordinary/large-output adoption remain required. No frozen-soak or release
success applies to this candidate.

Raw local evidence: `bench/results/command-allocation-local-2026-09-07.json.gz`.

Further inspection corrected two bounds before adoption: canonical pop records
reserve growth of the whole existing AOF buffer, including overlapping backing
allocations, rather than only the newly encoded record; reply reservations also
check the existing 192 MiB reply-class ceiling before construction. AOF-off pops
reserve their member arrays without charging an unused log. Tests verify refusal
before a tiny removal grows an already-full 1 MiB log, reply-class refusal when
the total budget still fits, and release/retry behavior. Three focused race and
slow-reader process repetitions, full tests and vet pass with both corrections.
Evidence: `command-allocation-classes-2026-09-07.json.gz`.

Review corrected peak accounting for execution runs with no successful reservation
and for later commands observing larger retained buffers. A focused regression
checks both paths and confirms refused memory is not counted as allocated.

Intel CI run 34162676036 failed the large collection restart fixture at its
five-second startup deadline. This was `TestRejectedCollectionPopsPreservePipelineAndRestart/barrier`,
not the historical pending-reply failure. Its log records replay of 195 commands
(roughly 195 MiB), followed by event-loop startup; the subsequently captured
stack is idle in kqueue. That is consistent with slow startup but does not show
the exact deadline state. This large-file fixture now has an explicit 30-second
recovery budget and records every readiness duration. Ordinary startup remains
five seconds and command idle deadlines are unchanged. Matched baseline/candidate
Intel repetitions will measure the recovery cost with identical assertions.
The original failed log remains in `command-allocation-review-2026-09-07.json.gz`;
a later pass is not a runtime fix or an explanation of older Mac observations.
