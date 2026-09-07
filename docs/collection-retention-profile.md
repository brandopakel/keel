# Hash and sorted-set retention profile — September 7, 2026

This is diagnostic evidence for the next memory change. It does not add serving
compaction or establish a latency improvement. The opt-in benchmark grows one
collection to 100,000 or 1,000,000 entries, deletes entries, forces GC, then replaces only
its Go map as a measurement probe. It retains the collection through the final
heap sample. Three isolated repetitions on an Apple M4 Pro produced the original 100,000-entry matrix:

| Collection | Survivors | Reclaimable map bytes, median |
| --- | ---: | ---: |
| Hash | 1 | 5,247,808 |
| Hash | 1,000 | 5,166,096 |
| Sorted set | 1 | 3,494,784 |
| Sorted set | 1,000 | 3,440,400 |

The full synchronous probe takes roughly 48–95 microseconds on this local
fixture. That is not an event-loop latency bound: larger high-water populations,
host scheduling, sparse-map traversal, concurrent logical mutations and transient
replacement-map allocations need separate treatment. Both current collection
structs are 24 bytes; adding tracking fields must be measured against many small
collections as well as large churned ones. Live-data memory estimates do not
measure retained Go map capacity or process RSS. This workload does not explain
memory differences in the unchanged string-based GoGIF pilot.

Reproduce with:

```sh
go test ./internal/data_structure -run '^$' -bench '^BenchmarkCollectionMapRetention$' -benchtime=1x -count=3
```

Raw attempts, exact toolchain/source and the benchmark are retained in
`bench/results/collection-retention-profile-2026-09-07.json.gz`. The first timing
probe included benchmark timer bookkeeping; only the corrected timing field
supports the figures above. Neither attempt is a production optimization.

An adoptable implementation must preserve hash/rewrite cursors and sorted-set
ordering under mutation, avoid an unbounded scan of sparse map storage, share
maintenance/admission budgets, survive persistence and replication checks, and
pass matched memory and throughput tests including small collections. Replacing
the map synchronously inside a command is not justified by this diagnostic alone.

The one-million-entry follow-up leaves much more retained capacity:

| Collection | Survivors | Reclaimable map bytes, median |
| --- | ---: | ---: |
| Hash | 1 | 83,805,760 |
| Hash | 1,000 | 83,683,056 |
| Sorted set | 1 | 55,783,920 |
| Sorted set | 1,000 | 55,756,816 |

Full synchronous rebuild probes now take about 1.68–1.90 ms, even when only one
entry survives. This exceeds the current cooperative maintenance target and is
why simply copying a sparse Go map in one maintenance call is insufficient.
These are retained-heap observations, not RSS, and map growth/randomization causes
variation. The benchmark's ns/op now covers the complete fixture so normal Go
benchmark calibration does not multiply expensive untimed population work; the
logged synchronous-probe interval still excludes population, deletion and GC.
Use the explicit one-iteration command above for comparable heap samples.
The 24-case follow-up, source and earlier timing attempt are retained in
`bench/results/collection-retention-scale-2026-09-07.json.gz`.
