# Reserving a transcript for eviction

Status: costed and **not recommended**, September 7, 2026. Nothing here is
implemented. The current behaviour is correct; this records why the obvious
improvement to it is worse than it looks.

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

Refused means the drained barrier: correct, and slower than it might be. The
comment invites the improvement, so it deserves an answer rather than standing
open indefinitely.

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

Sound, and it moves the work rather than removing it. The eviction still happens
on the loop; the run that follows overlaps with the append instead of waiting for
it. The gain is real but small, and it is bought with a preflight pass over the
keyspace that runs whether or not the estimate was needed.

It also does not fully close the case: `growth` is a conservative
over-estimate, so a run admitted this way can still find itself short and evict
anyway.

### C. Reserve the worst case

`bytesToFree / smallestKeyBytes × maxDelRecord`. Correct and useless: on a
keyspace of small keys the reservation exceeds any sane budget, so every run
that might evict is refused — which is what happens now, with more arithmetic.

### D. Keep the barrier

What the code does.

## Recommendation: D

The condition this optimises is a server *at* its memory limit. Such a server is
evicting on a large share of writes, which means it is already doing the work of
choosing victims, writing removal records and updating accounting on the loop.
The append barrier is not what is limiting it, and removing the barrier would not
make it fast — it would make it slightly less slow while it is in trouble.

The honest framing: **concurrent append is an optimisation for a healthy server,
and a server at its eviction limit is not one.** Effort is better spent on making
that state rarer or more visible than on shaving a barrier out of it.

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
