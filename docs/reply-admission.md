# Amplified read replies

A request repeating a 1 MiB string, hash field or set member 65 times constructed
a 68,158,225-byte response before the server noticed its 64 MiB output limit.
The previous MGET/HMGET encoder also constructed individual encoded values and
grew a second combined buffer. The three reproduced regression cases are kept
with their before/after output in `bench/results/reply-admission-2026-09-07.txt`.

MGET and HMGET now size their complete RESP reply before allocating it, then
encode directly into one buffer. Framing and repeated fields count separately;
missing values, empty strings and binary strings keep their existing wire forms.
Negative-count SRANDMEMBER fixes its draw in a bounded index array, sizes the
exact selected payload, and encodes it directly. Both that index storage and the
encoded response have a 64 MiB ceiling. The set's contents/order do not change.

An oversized result returns `ERR reply exceeds the 64 MiB output limit`. This is
Keel's resource limit, not a Redis compatibility claim for oversized responses.
The connection can continue processing its pipeline. The checks apply with AOF
off, asynchronous barrier appends and concurrent appends. Process tests verify
subsequent PING/GET replies and persistence after restart; rejected reads leave
the values intact. All three amplified-response regressions reject with less
than 256 KiB allocated, rather than allocating the 65 MiB payload.

The complete local Go suite and vet passed; additional focused framing,
binary/nil, index-ceiling and process/restart checks pass. Hosted validation is
pending. Whole-collection reads, mutating pop commands, aggregate transient
allocation admission and per-client scheduling still need their own work. This
change does not establish a process RSS ceiling or a command latency bound.
