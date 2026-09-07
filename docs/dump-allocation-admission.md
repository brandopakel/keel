# Dump admission and encoding

A 65-element list sharing one 1 MiB string produced a 68,157,722-byte KEEL.DUMP
reply and allocated 629,111,464 bytes along the way. The previous command built
member arrays, growing payload buffers, version/checksum copies and an encoded
RESP copy before the server could reject output above 64 MiB.

The command now computes exact body and RESP framing sizes before allocating,
stops collection sizing at the limit, and encodes accepted payloads directly
into one sized output buffer. Nonmaterializing walkers avoid member arrays.
The five opaque serializers expose exact sizes and append into supplied storage;
ordinary Marshal callers also preallocate their final size. Dump wire format,
KEL1 version/checksum and legacy payload support remain unchanged.

The oversized-list regression now returns the existing output-limit error with
32 bytes allocated. A 4 MiB accepted list dump decreases from 28,775,976 to about
4,202,560 bytes allocated. A small CMS initialization command can create a table
above the reply limit; that dump is refused before marshalling. These are local
allocation measurements, not throughput or deployment latency claims.

Ten payloads captured from the pre-change 7fc360d9 runtime cover strings, hashes,
lists, sets, sorted sets and all five opaque types. Restore/re-encode preserves
their bytes and complete RESP framing. The fixture retains binary identity in
internal/core/testdata/dump-legacy-7fc360d.json. Full local tests/vet, focused core
race and serializer/HLL behavior race checks pass. A broad race subset exposed
an existing small-population HLL heap-test failure, reproduced on unchanged
baseline and passing in isolation on both; its measurement fix is separate. All
raw attempts are preserved in bench/results/dump-allocation-2026-09-07.json.gz.

DUMP remains an atomic, potentially expensive command. Accepted large replies
can still take proportional CPU; total live-process allocation is not bounded
by one reply's limit. Opaque rewrite serialization, file writes and final fsync
still have synchronous work. The internal persistence dump path preserves its
existing limits and is not silently capped at the client response ceiling.
