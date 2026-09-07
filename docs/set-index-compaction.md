# Set membership map compaction — September 7, 2026

Shrinking the dense member slice did not shrink a set’s membership map. A set
that grew to 100,000 members and retained 1,000 still held about 3.4 MiB in that
sparse map. The candidate begins a shadow-map rebuild when a sufficiently large
member slice reaches its existing shrink threshold. Maintenance copies bounded
positions from the dense slice; insertions, last-member moves, deletions and
random shuffles mirror their positions into the shadow map. The old map stays
authoritative until completion. Substantial regrowth cancels the rebuild.

The compactor adds one pointer to Set (40 to 48 bytes; both occupy Go’s 48-byte
allocation class on tested 64-bit builds), with no per-member metadata. Large
shrink events allocate the replacement state lazily. Collection maintenance,
TTL compaction and key lookup compaction rotate through one shared 1,024-work
budget and cooperative 1 ms target. Visiting a key/empty page and copying a
member each consume work. Logical membership and order do not change, so no
persistence record is emitted. Existing member-slice shrink copies remain
synchronous; this does not bound every collection command’s latency.

The 100,000-to-1,000 fixture retains its map on baseline (32 bytes of noise),
while the candidate recovers 3,440,296 bytes in the race run. Mutation/shuffle,
regrowth, multiple-set budget and persistence/replay regressions pass. Full
local tests/vet pass. Raw baseline failures and candidate evidence are in
`bench/results/set-index-compaction-2026-09-07.json.gz`. Hosted compatibility,
small-set memory and throughput adoption remain pending. Hash/ sorted-set maps
and partially occupied key pages are separate remaining memory work.

Review closeout strengthens the serving-maintenance test: the fixture must
start with a pending rebuild, and the scheduled hook must finish it. This fails
if the collection hook is removed. The live member sequence is checked exactly
before/after maintenance. Replay deliberately checks unordered membership;
set iteration order across restarts is not a client contract. A further large
shrink restarts the shadow map to avoid retaining its own high-water capacity.
