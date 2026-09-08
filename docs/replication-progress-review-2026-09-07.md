# Replication progress metric correction

PR 59 introduced a greatest-reported-offset metric. Review reproduced four
problems against `855120b`: a future cursor advanced it beyond the stream end;
a lower cursor refreshed the timestamp of a higher cursor; malformed delta pulls
refreshed it; snapshot-transfer requests also refreshed it. Each new regression
failed before the correction. This affects telemetry, not the replication data path.

Record the requested cursor only after constructing a valid ordinary same-epoch
delta response. Update the greatest cursor and its timestamp together only when
the new cursor reaches or exceeds it. Out-of-history cursors, syntax failures,
snapshot transfers and stream errors do not record progress. INFO field names
remain compatible.

A frame cursor may include a partial command in the replica's pending buffer.
It is received-byte progress, not a verified applied or durable prefix. The largest
cursor does not identify a current replica, establish a quorum, or quantify data
loss on promotion. The API comments and replication documentation now state that
scope. Per-replica durable acknowledgments remain separate engineering.

The failing check finished in 6.72 seconds; the corrected regressions, prior metric
checks and targeted protocol-2 tests passed in 5.77 seconds. Both local invocations
used the resource guard and pruned compilation caches. Exact source/patch, failure
and passing output are in `bench/results/replication-progress-regressions-2026-09-07.json.gz`.
Hosted full/race/native and filesystem checks remain required before merge.

Review also found that epoch invalidation reset the stream without clearing the
old progress metric. It now resets the cursor and age together. The regression
first proves that a valid cursor was recorded, changes the epoch, checks unknown
progress, and confirms a fresh zero cursor can be observed. Rejection tests now
also verify the stored offset before and after each rejected request.

The first validation attempt was interrupted by a resource-guard directory-removal
race; its partial output is preserved. With the guard correction, the complete
targeted metric/protocol-2 suite passes in 5.74 seconds. Both outcomes and the exact
patch are in `bench/results/replication-progress-epoch-review-2026-09-07.json.gz`.
