# Reserving a transcript for eviction

Status: design options, September 7, 2026. Eviction reservations are not
implemented. Keep the current drained append barrier until another path has a
complete reservation/recovery contract and demonstrates a measured benefit.

## What happens today

`AppendAdmission` refuses a run that might evict:

```go
if data_structure.TotalKeys()+newKeys > config.KeyNumberLimit ||
    (config.MaxMemory > 0 && data_structure.TotalMemUsed()+growth > config.MaxMemory) {
    // An eviction can name an arbitrary old key. Until its transcript has
    // its own reservation, runs that may evict wait for the barrier.
    return 0, 0, false
}
```

Refused means draining pending appends and executing this run exclusively.
That supplies ordering; it does not prove that every temporary allocation is
reserved before construction. The barrier's cost under sustained eviction
still needs measurement.

## Why the reservation is hard

Admission has to bound the log record a run produces *before* it runs. For a
command that is straightforward: the record is derivable from the arguments and
the keyspace. For eviction it is not.

Eviction emits a `DEL` for each key it removes, and neither the count nor the
names are knowable in advance. The count depends on how many bytes must be
freed and on the sizes of whichever keys the policy happens to choose; the names
are chosen from the whole keyspace by a policy weighing access recency or
frequency. Freeing one megabyte might mean one `DEL` or fifty thousand.

That is the whole difficulty. A bound needs a worst case, and the worst case here
is "bytes to free, divided by the smallest key in the keyspace" — which for a
keyspace of tiny keys is not a bound worth having.

## Options

### A. Cap evictions inside an admitted run

Reserve `N` removal records, and stop evicting at `N`.

This is the tempting one and it is unsafe. Eviction is not optional work: it is
what keeps the server inside `maxmemory`. A run that needs to evict sixty keys
and is allowed twenty leaves the budget breached, and the budget is the promise.
Trading a memory bound for concurrency is the wrong trade, and it fails in the
state where being wrong matters most.

Aborting mid-run instead is not available either — the commands have executed by
the time the shortfall is known, which is the failure admission exists to
prevent.

### B. Evict before admitting

Free the space on the loop first, recording the actual transcript, then admit the
run with no eviction expected.

This needs an ordered removal transcript and a proven growth bound before
admission. A conservative bound is sufficient only if it includes every source
of growth; omitted map, collection or metadata allocation is a defect in the
model. Pre-eviction may remove more useful data than the command ultimately
needs. Measure cache misses, bytes, latency and throughput before claiming a
gain or deciding its size.

### C. Reserve the worst case

A useful bound needs both victim count and name bytes, including lazy expiry
and canonical side effects. A bound larger than the queue budget safely falls
back to the barrier. Whether a tighter bound is worthwhile depends on the
workload and eviction policy; the simple size-ratio estimate is not a proof.

### D. Keep the barrier

What the code does.

## Recommendation: D

Running a cache at `maxmemory` is normal. No evidence in this proposal shows
that victim selection dominates the append barrier or that eliminating the
barrier would have only a small benefit. Retaining the barrier is a correctness
fallback pending a complete alternative, not a conclusion that full-cache
workloads are unhealthy or unimportant.

## What would change the answer

- A measurement showing the barrier is a meaningful share of latency for a
  workload that runs at its `maxmemory` bound deliberately, rather than as a
  symptom of being under-provisioned. Caches often do run full on purpose, so
  this is a real possibility rather than a rhetorical one — it has simply not
  been measured.
- A policy whose victim count is bounded by construction — evicting whole
  size-classes, say — which would give the reservation a worst case worth having.

Either would make option B worth building. Neither is established today, and the
comment in `aof_admission.go` should point here rather than imply the work is
merely pending.

Compare tiny keys, mixed sizes, skewed access and large collections under
identical no/everysec/always policies. Record eviction time, transcript bytes,
queue/barrier occupancy, acknowledgment latency, cache misses, allocation peaks,
dropped and failed requests. Correctness gates include refused admission without
partial command effects, long victim names, expiry, ordered replies, storage
failure, rewrite and restart/replica recovery.
