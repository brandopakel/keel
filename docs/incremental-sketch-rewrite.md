# Incremental CMS and Morris rewrite encoding

Status: candidate with qualified hosted results, September 7, 2026.

CMS and Morris tables have a fixed shape after creation. A rewrite can capture
their small header and borrow their counter storage, then encode at most 64 KiB
per event-loop slice. The same owner runs commands and encodes each piece;
there are no concurrent reads of mutable cells. The CRC covers the bytes
actually emitted, including pieces whose cells changed after an earlier slice.

Such an intermediate image is structurally valid but can mix counter values
from different command boundaries. It is never published as a final snapshot
without reconciliation. Every intervening write marks the key dirty. The stream
finishes its RESP record, then a complete replacement records the exact final
table, total count and Morris RNG state. Mutation during that replacement marks
it dirty again. Existing dirty-name/byte/time limits abort excessive churn.
Deletion or replacement retains the old borrowed table until the record finishes
or the whole rewrite aborts; that retained storage is a remaining memory cost.

Dynamic Bloom/cuckoo filters and HLL keep their existing immutable-image path.
This does not introduce concurrent command execution, remove file-write or final
sync/rename pauses, or make large replication deltas incremental. A worker must
never receive the borrowed sketch reader. The on-disk KEL1 format is unchanged.

Five local first-slice diagnostics compare the same benchmark source against
pre-term baseline `a7c6600cece5d21a78794e40e2f59be0e55028d4`, using Go 1.26.6 on
Apple Silicon. Starting either a 4 MiB or 64 MiB sketch record allocates about
66 KB. Baseline starts allocate about 4.27 MB and 67.18 MB respectively. Baseline
64 MiB CMS startup takes roughly 40–46 ms; candidate observations are roughly
20–46 microseconds. These are single-iteration diagnostic samples on a Mac
running frozen soaks and other validation, not an end-to-end latency guarantee.
The first prototype unnecessarily grew its output slice repeatedly; one reserved
slice reduces first-cycle allocation from about 286 KB to 66 KB.

Boundary tests compare the existing encoding byte for byte, including chunks
that split table headers, counter words and CRC bytes. Tests mutate the table
between 32 body slices, then verify exact payload and TTL through two replays.
Delete/replace tests retain checksum-valid intermediate records. Three focused
race repetitions, the full local suite and vet pass.

The matched rewrite harness now supports strings, CMS and Morris as independent
datasets. Each hosted dataset runs 36 rotated arms across no/everysec/always and
0/20% competing writes, with 32 MiB snapshots and independent scheduled arrivals.
Local smoke runs retain any dropped requests and do not satisfy the hosted
adoption gate. A benchmark with traffic on other keys does not establish progress
under constant mutation of the large sketch itself; the bounded abort contract
and mutation correctness tests cover that separate behavior.

Hosted run 34155169517 compares `440baca` with `a7c6600`, 108 arms in total.
The Morris and string datasets each complete 108 rewrites and 1,080,000 requests
with zero failed, dropped or expired requests. Across the six Morris policy/write
combinations, median scheduled p99.9 falls from 6.16–9.31 ms to 1.23–4.16 ms.
String p99.9 varies: the no-write no-fsync case rises 4.10 → 4.46 ms, while the
always-sync 20%-write case falls 10.62 → 6.49 ms. Ordinary string tail improvement
is not established by these short three-pair cells.

The CMS runner drops 18,992 scheduled requests in ten arms across both versions.
There are no server-command errors, but this is not a complete offered workload
and does not satisfy adoption. Its green workflow conclusion originally meant
only that all processes and rewrites completed. The harness now preserves traffic
in failed reports and refuses adoption on any failed, dropped or expired request;
all 108 retained traffic records check that rule, including the ten overload arms.
A separately labeled 500-request/s run is requested to measure interference below
that offered load. It cannot erase the 2,000-request/s overload evidence or
establish a capacity guarantee.

Full raw reports and host records are retained in
`bench/results/incremental-sketch-rewrite-hosted-2026-09-07.json.gz`. Public VM
storage/tenancy remains uncontrolled, and the paired baseline predates term
guards. No release or frozen-soak success is attributed to this candidate. It
still needs integrated native validation and assessment of the failed arms below.

Run 34157654209 measures 500 requests/s at the same serving runtime (`c4481b7`
changes only harness inputs). CMS and strings each complete 36 arms, 108 rewrites
and 270,000 scheduled requests without errors or drops. CMS median scheduled
p99.9 improves from 15.20–16.38 ms to 1.26–2.62 ms across the six combinations.
The Morris matrix drops 1,203 requests in seven always-sync arms across both
versions. The affected observed service-time maxima reach 146–395 ms while
scheduler-lag maxima remain around 1–2 ms; this distinguishes waiting for service
from generator scheduling, but does not identify the responsible filesystem or
runtime call. Those arms fail the stricter gate and remain open operational
evidence. The earlier complete Morris matrix is retained alongside this failure.
Repeatedly lowering offered load until a public VM passes would not explain it.

Run 34156908649 never started the rewrite jobs: GitHub rejected the numeric input
passed through the reusable workflow. The rate is now a string, parsed and bounded
by the harness; 34157654209 confirms all three matrices execute. Full evidence is
in `bench/results/incremental-sketch-rewrite-500-2026-09-07.json.gz`.
