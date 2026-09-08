# GEOSEARCH allocation admission

Status: merged in PR 53, unreleased, September 7, 2026.

GEOSEARCH previously collected all matching points, sorted the full collection
for COUNT, and encoded replies before applying the output limit. Six 11 MiB
member names allocated about 69.2 MB before refusal. COUNT 1 over 50,000 matches
allocated about 13.8 MB to return one member.

Unordered searches now walk matches twice: first to size the exact RESP reply,
then to encode into one reserved output slice. Ordered searches retain at most
COUNT selected points in a heap, then sort the selected points. ANY still stops
after the first matching COUNT points; an explicit order sorts that selection.
Sorting has a separate 64 MiB point-workspace ceiling. Every encoded reply has
the existing 64 MiB output ceiling, checked before construction. The equivalent
local regressions allocate about 256 and 464 bytes respectively.

This is per-command admission, not a process-wide temporary-allocation pool.
An ordered search can retain both point workspace and its encoded reply. Nearest
selection still examines every candidate match, so CPU work grows with the set
and long member names. There is no fixed command-latency guarantee. GEOHASH and
GEOPOS retain their existing response paths. Stored scores and persistence format
are unchanged.

Tests compare bounded selection with an independent full distance sort over
1,000 points, including nearest/farthest, small and oversized COUNT arguments,
ANY, all eight reply-option combinations, and exact RESP encoding. The full Go
suite, vet and three focused race repetitions pass on Apple Silicon Go 1.26.6.
The Redis differential harness adds 360 GEO comparisons per pass, including
global-radius membership, and repeats them through rewrite and two restarts:
1,080 comparisons pass alongside 10,000 ordinary mixed commands.

Three benchmark scenarios use one verified 50,000-member set: nearest COUNT 1,
nearest COUNT 100, and COUNT 1 ANY. A two-second local smoke passes every arm but
does not qualify performance on the Mac running frozen soaks. Matched repeated
measurements include ordinary small reads and pipeline traffic to assess costs.
Raw local evidence is retained in
`bench/results/geo-response-admission-local-2026-09-07.json.gz`. No release or
frozen-soak success is attributed to this candidate.

Hosted run 34157080059 compares `e2644b6` with `c235be7`, five alternating
15-second repetitions per case and disjoint exposed CPU groups. All 50 arms
complete without client CPU warnings:

| Workload | Paired throughput ratio, median (range) | Median p99 ms, baseline → candidate | Median RSS MiB, baseline → candidate |
| --- | ---: | ---: | ---: |
| Small string read | 1.002 (0.991–1.030) | 0.327 → 0.327 | 14.91 → 14.60 |
| Pipeline 64 | 1.011 (0.998–1.016) | 2.039 → 2.191 | 14.47 → 14.43 |
| GEO nearest 1 | 2.246 (2.230–2.264) | 327.679 → 131.071 | 39.89 → 19.36 |
| GEO nearest 100 | 2.234 (2.206–2.271) | 301.055 → 124.415 | 39.25 → 21.37 |
| GEO ANY 1 | 0.985 (0.973–0.993) | 0.383 → 0.391 | 26.42 → 25.70 |

Nearest selection improves substantially for this 50,000-match case; its command
work still scales with matches. ANY is roughly 1.5% slower, and pipeline p99 is
about 7.5% higher despite slightly higher throughput. Those costs are retained,
not described as improvements. Public VM host scheduling and storage remain
uncontrolled. The adoption tradeoff favors bounded allocation and the demonstrated
nearest-search gain while accepting the small ANY throughput cost. The ordinary
pipeline tail difference needs assessment again in the integrated comparison.
Raw evidence is in `bench/results/geo-response-admission-matched-2026-09-07.json.gz`.

Review found that a large COUNT within the point limit reserved space before
filtering the shape. A 50,001-member fixture with one matching location allocated
2,400,672 bytes for COUNT 50,000. Ordered counts above 1,024 now count matching
points up to the selection/workspace bound before allocating once. The same
fixture allocates 488 bytes for COUNT 50,000, 1,000,000 and 10,000,000. This avoids
slice growth temporarily retaining two large arrays; large-count queries pay an
additional bounded membership pass. COUNT 1/100 benchmark paths are unchanged.

The unused range collector is removed. Differential checks now enforce option
and coordinate cardinality and test COUNT ANY by membership, count and encoded
fields without requiring identical subsets. The expanded 10,000-command run,
rewrite and two restarts pass, as do the full local suite, vet and three focused
race repetitions after integrating bounded hash/zset storage and term guards.
Raw review validation is in `geo-response-review-2026-09-07.json.gz`.

Final corrected candidate `e93d978` passes the hosted Go/race/Docker matrix,
external clients, Redis differential/native smoke, Linux ARM64 and Intel Mac
recovery, and ext4/xfs recovery. The expanded local differential records 1,296
GEO comparisons across initial state, rewrite and two restarts. The large-count
fix uses a membership prepass rather than slice growth, preserving one bounded
point allocation. PR 53 merged after those checks. The retained CodeRabbit review covers the
initial candidate; the subsequent sparse-count correction has local regression
and differential evidence, but no separate completed CodeRabbit follow-up is
recorded on that PR.
