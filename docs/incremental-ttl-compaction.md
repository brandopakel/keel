# Incremental compaction of nonempty TTL tables

The September 7 regression grew an expiry table to 100,000 entries and cleared
99,000 TTLs while keeping the values. The remaining 1,000 TTLs left roughly
3.44 MB of avoidable live heap in both string and collection stores. Releasing
only completely empty tables did not address this case.

A store whose expiry table peaked at 1,024 entries or more can start rebuilding
when occupancy falls to one quarter of that peak. The old table remains
authoritative until the captured stable-slot traversal ends. SET/PEXPIRE/PERSIST,
overwrite, deletion and slot reuse mirror their TTL changes into the replacement,
including new keys behind the cursor or beyond its captured end. Growth past
half the old peak cancels the replacement; empty tables release both copies.
The map grows gradually without a whole-table allocation hint or key-name list.

The event loop advances memory maintenance once per second on its existing
maintenance schedule, including the pending-append path and replicas. Each turn
uses at most 1,024 traversal work units across rotating stores, with a cooperative
one-millisecond target. Compaction does not delete logical keys or produce AOF
records. It is separate from primary-only active expiry. Existing stable SCAN
slots stay in place.

The before/after heap checks recovered 3.44 MB and returned within 512 KiB of the
original 1,000-TTL baseline. Small single-key TTL churn still performs zero map
allocations per clear/reapply cycle. Tests interleave changes on either side of
the cursor, reuse freed slots, append beyond the snapshot end, cancel on regrowth
and clear during compaction. A 420,000-key fixture verifies starting compaction
allocates less than 256 KiB and honors a 37-unit step. Replica maintenance preserves
keys, TTLs and logical memory accounting. Full local Go tests and vet pass.

This measures live heap, not immediate RSS return to the OS. Two tables coexist
while copying, so temporary memory and extra map writes are required. Completion
can take many maintenance turns, especially when persistent keys greatly
outnumber TTLs. There is no fixed latency or completion-time guarantee; scheduling,
GC and an individual map allocation can exceed the cooperative target. Sparse
key pages, lookup-map capacity and collection-map capacity remain separate work.
