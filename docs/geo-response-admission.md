# GEOSEARCH allocation admission

Status: candidate awaiting matched validation and review, September 7, 2026.

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
