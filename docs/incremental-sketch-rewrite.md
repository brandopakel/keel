# Incremental CMS and Morris rewrite encoding

Status: candidate awaiting hosted adoption, September 7, 2026.

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

No release or frozen-soak success is attributed to this candidate. It needs
review, matched interference/adoption results and integrated native validation.
