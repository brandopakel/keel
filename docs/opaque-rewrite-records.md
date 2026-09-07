# Opaque rewrite record copy reduction — September 7, 2026

A 4 MiB count-min sketch previously allocated 12,608,840 bytes on its first
rewrite advance and emitted a 4,194,375-byte RESP record without yielding.
The candidate retains one immutable KEL1 image directly and streams its RESP
framing, key name, payload and expiry in 64 KiB fragments. The reproduced
first advance allocates 4,268,792 bytes and emits 65,536 bytes. Small opaque
records keep their existing batched path.

A mutable sketch still requires an immutable full image: changing live state
between fragments must not invalidate the checksum or structure of an earlier
RESTORE record. That historical record finishes before dirty reconciliation
replaces it. The candidate removes the additional payload-to-string and whole
RESP copies; it does not make initial serialization incremental or cap the size
of a pre-existing opaque value. Internal persistence remains readable for
values exceeding the client DUMP reply limit.

Twenty mutation cases cover Bloom, CMS, Morris, HLL and Cuckoo state, with
long key names, expiry, update/replacement/deletion during a partial record
and two complete replays. Every historical payload must parse and validate
before its final replacement. Existing string/collection streaming tests,
focused race, full tests and vet pass. Raw failed-baseline and passing-candidate
logs are retained in `bench/results/opaque-rewrite-records-2026-09-07.json.gz`.
Hosted review and correctness checks remain pending. These allocation results
are local regression measurements, not deployment latency or throughput claims.
